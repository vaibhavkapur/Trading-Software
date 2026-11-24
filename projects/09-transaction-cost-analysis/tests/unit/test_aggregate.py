from decimal import Decimal as D

import pytest
from builders import at, child, fill, order, quote, run

from tca.aggregate import bucket_label, summarize
from tca.benchmark import ARRIVAL_MID, DECISION_MID

# A flat market: 100.00 / 100.04 (mid 100.02) refreshed every two seconds on one venue.
FLAT = tuple(quote("100.00", "100.04", second) for second in range(-4, 121, 2))


def by_key(groups):
    return {group.key: group for group in groups}


def close(left, right) -> bool:
    """Equal up to the last digits of 28-digit decimal division."""
    return abs(left - right) < D("1e-18")


def test_group_bps_is_cost_over_notional_not_a_mean_of_order_bps():
    analysis = run(
        [order("SMALL", qty=100), order("BIG", qty=10_000)],
        [
            fill("F1", 100, "100.22", 1, order_id="SMALL"),  # 20c worse: ~20 bps
            fill("F2", 10_000, "100.03", 1, order_id="BIG"),  # 1c worse: ~1 bps
        ],
        FLAT,
    )
    (total,) = summarize(analysis.results)
    assert total.slippage_cost == D("120.00")  # 100 * 0.20 + 10,000 * 0.01
    assert total.benchmark_notional == D("1010202.00")  # 10,100 * 100.02
    assert total.slippage_bps == D("120") / D("1010202") * 10_000
    assert round(total.slippage_bps, 4) == D("1.1879")  # a plain mean would say ~10.5
    assert (total.orders, total.orders_filled, total.fills) == (2, 2, 2)
    assert total.fill_rate == 1


def test_untrusted_orders_are_counted_but_left_out_of_costs_by_default():
    analysis = run(
        [order("GOOD"), order("STALE", arrival=100), order("EMPTY", arrival=2)],
        [
            fill("F1", 1000, "100.04", 1, order_id="GOOD", fee="1.00"),
            fill("F2", 1000, "100.14", 101, order_id="STALE", fee="1.00"),
        ],
        [q for q in FLAT if q.ts <= at(60)],  # feed stops 40s before STALE arrives
    )
    (default,) = summarize(analysis.results)
    assert (default.orders, default.orders_filled) == (3, 2)
    assert (default.orders_trusted, default.orders_costed) == (1, 1)
    assert default.trusted_share == D("0.5")
    assert default.slippage_cost == D("20.00")
    assert default.costed_fees == D("1.00")
    assert default.fees == D("2.00")  # fees paid are reported for every fill regardless
    assert default.fill_rate == D(2000) / 3000

    (everything,) = summarize(analysis.results, include_untrusted=True)
    assert (everything.orders_trusted, everything.orders_costed) == (1, 2)
    assert everything.slippage_cost == D("140.00")
    assert everything.costed_fees == D("2.00")


def test_components_add_up_to_slippage_and_all_in_cost():
    analysis = run(
        [order("A"), order("B", side="sell", arrival=10), order("C", arrival=20)],
        [
            fill("A1", 600, "100.05", 3, order_id="A", fee="1.80"),
            fill("A2", 400, "100.03", 7, order_id="A", fee="-0.80"),
            fill("B1", 1000, "99.99", 13, order_id="B", fee="3.00"),
            fill("C1", 1000, "100.06", 23, order_id="C", fee="3.00"),
        ],
        FLAT,
    )
    (total,) = summarize(analysis.results)
    assert total.orders_decomposed == 3 and total.unsplit_cost == 0
    assert total.spread_cost + total.drift_cost + total.unsplit_cost == total.slippage_cost
    assert close(total.spread_bps + total.drift_bps + total.unsplit_bps, total.slippage_bps)
    assert close(total.slippage_bps + total.fee_bps, total.total_bps)
    assert total.total_cost == total.slippage_cost + D("7.00")
    assert total.drift_cost == 0  # the mid never moved


def test_orders_that_cannot_be_split_go_to_an_explicit_unsplit_bucket():
    quotes = [quote("100.00", "100.04", -1), quote("100.00", "100.04", 9)]
    analysis = run(
        [order("SPLIT", arrival=10), order("UNSPLIT")],
        [
            fill("F1", 1000, "100.05", 11, order_id="SPLIT"),
            fill("F2", 1000, "100.08", 8, order_id="UNSPLIT"),  # quote is 9s old at the fill
        ],
        quotes,
    )
    (total,) = summarize(analysis.results)
    assert (total.orders_costed, total.orders_decomposed) == (2, 1)
    assert total.slippage_cost == D("90.00")
    assert (total.spread_cost, total.drift_cost, total.unsplit_cost) == (D("30.00"), 0, D("60.00"))
    assert close(total.spread_bps + total.drift_bps + total.unsplit_bps, total.slippage_bps)


def test_split_is_only_reported_against_arrival():
    analysis = run([order()], [fill("F1", 1000, "100.05", 3)], FLAT)
    (decision,) = summarize(analysis.results, benchmark=DECISION_MID)
    assert decision.orders_costed == 1 and decision.orders_decomposed == 0
    assert decision.spread_bps is None and decision.unsplit_bps is None


def test_effective_spread_is_measured_against_the_mid_at_each_fill():
    analysis = run([order()], [fill("F1", 1000, "100.04", 3)], FLAT)
    (total,) = summarize(analysis.results)
    assert close(total.effective_spread_bps, 2 * D("0.02") / D("100.02") * 10_000)
    assert close(total.avg_quoted_spread_bps, D("0.04") / D("100.02") * 10_000)


def test_venue_rows_split_an_order_without_losing_or_double_counting_cost():
    analysis = run(
        [order()],
        [
            fill("F1", 300, "100.04", 3, venue="V1", child_id="C1", fee="0.90"),
            fill("F2", 500, "100.06", 5, venue="V2", child_id="C2", fee="0.50"),
        ],
        FLAT,
        child_orders=[
            child("C1", "V1", 300, 2),
            child("C2", "V2", 500, 4),
            child("C3", "V2", 200, 6),  # never filled
            child("C4", "V3", 100, 8),  # routed to a venue that never filled anything
        ],
    )
    venues = by_key(summarize(analysis.results, "venue"))
    (total,) = summarize(analysis.results)
    assert set(venues) == {"V1", "V2", "V3"}
    assert venues["V1"].slippage_cost == D("6.00")  # 300 * 0.02
    assert venues["V2"].slippage_cost == D("20.00")  # 500 * 0.04
    assert sum(v.slippage_cost for v in venues.values()) == total.slippage_cost
    assert sum(v.fees for v in venues.values()) == total.fees
    assert sum(v.filled_qty for v in venues.values()) == total.filled_qty

    assert venues["V1"].fill_rate == 1  # 300 filled of 300 routed
    assert venues["V2"].fill_rate == D(500) / 700
    assert venues["V3"].fill_rate == 0 and venues["V3"].orders_filled == 0
    assert venues["V3"].slippage_bps is None and venues["V3"].trusted_share is None
    assert venues["V1"].avg_time_to_first_fill_ms is None  # not attributed to venues
    assert total.fill_rate == D("0.8")


def test_venue_fill_rate_is_unknown_without_child_orders():
    analysis = run([order()], [fill("F1", 300, "100.04", 3)], FLAT)
    (venue,) = summarize(analysis.results, "venue")
    assert venue.routed_qty == 0 and venue.fill_rate is None


def test_order_level_dimensions():
    analysis = run(
        [
            order("A", broker="B1", strategy="TWAP"),
            order("B", side="sell", broker="B2", strategy="TWAP", arrival=10),
            order("C", broker="B1", strategy="VWAP", symbol="ABC", arrival=20),
        ],
        [
            fill("F1", 1000, "100.05", 3, order_id="A"),
            fill("F2", 1000, "99.99", 13, order_id="B"),
        ],
        FLAT,
    )
    results = analysis.results
    assert {k: g.orders for k, g in by_key(summarize(results, "broker")).items()} == {
        "B1": 2,
        "B2": 1,
    }
    assert list(by_key(summarize(results, "strategy"))) == ["TWAP", "VWAP"]
    assert list(by_key(summarize(results, "symbol"))) == ["ABC", "XYZ"]
    sides = by_key(summarize(results, "side"))
    assert sides["buy"].slippage_cost == D("30.00") and sides["sell"].slippage_cost == D("30.00")
    assert sides["buy"].fill_rate == D("0.5")


def test_time_to_fill_averages_over_filled_orders():
    analysis = run(
        [order("A"), order("B", arrival=10), order("C", arrival=20)],
        [
            fill("A1", 500, "100.04", 2, order_id="A"),
            fill("A2", 500, "100.04", 6, order_id="A"),
            fill("B1", 1000, "100.04", 14, order_id="B"),
        ],
        FLAT,
    )
    (total,) = summarize(analysis.results)
    assert total.avg_time_to_first_fill_ms == 3000  # (2s + 4s) / 2
    assert total.avg_time_to_last_fill_ms == 5000  # (6s + 4s) / 2


def test_time_buckets_group_by_arrival():
    assert bucket_label(at(0), 30) == "2026-03-02T15:00Z"
    assert bucket_label(at(29 * 60 + 59.999999), 30) == "2026-03-02T15:00Z"
    assert bucket_label(at(30 * 60), 30) == "2026-03-02T15:30Z"
    assert bucket_label(at(-1), 60) == "2026-03-02T14:00Z"
    assert bucket_label(at(7 * 60), 5) == "2026-03-02T15:05Z"

    analysis = run([order("A"), order("B", arrival=45 * 60), order("C", arrival=50 * 60)])
    buckets = summarize(analysis.results, "time_bucket", bucket_minutes=30)
    assert [(g.key, g.orders) for g in buckets] == [
        ("2026-03-02T15:00Z", 1),
        ("2026-03-02T15:30Z", 2),
    ]


def test_empty_groups_report_nothing_rather_than_zero():
    assert summarize([]) == []
    (total,) = summarize(run([order()]).results)
    assert total.orders == 1 and total.orders_filled == 0
    for value in (
        total.slippage_bps,
        total.fee_bps,
        total.total_bps,
        total.total_cost,
        total.trusted_share,
        total.effective_spread_bps,
        total.avg_time_to_first_fill_ms,
    ):
        assert value is None
    assert total.fill_rate == 0


def test_invalid_arguments():
    with pytest.raises(ValueError, match="group_by"):
        summarize([], "desk")
    with pytest.raises(ValueError, match="bucket_minutes"):
        summarize([], "time_bucket", bucket_minutes=0)
    assert summarize([], benchmark=ARRIVAL_MID) == []
