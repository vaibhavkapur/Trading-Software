"""Per-order cost formulas, checked against figures worked out by hand.

The worked example used throughout (also in docs/methodology.md and examples/worked/):

    arrival 15:00:00, consolidated quote 100.00 / 100.04  -> arrival mid 100.02
    fill 1  15:00:05, quote 100.01 / 100.05               -> mid 100.03
    fill 2  15:01:20, quote 100.06 / 100.10               -> mid 100.08
"""

import random
from decimal import Decimal as D

import pytest
from builders import at, bar, child, fill, order, quote, run

from tca.attribution import BPS
from tca.benchmark import ARRIVAL_MID, DECISION_MID, DEGRADED, INTERVAL_VWAP, MISSING, OK
from tca.domain import Side

QUOTES = (
    quote("99.98", "100.02", -3),
    quote("100.00", "100.04", -1),
    quote("99.99", "100.05", -0.5, venue="V2"),
    quote("100.01", "100.05", 4),
    quote("100.00", "100.06", 4.5, venue="V2"),
    quote("100.06", "100.10", 79),
    quote("100.05", "100.11", 79.5, venue="V2"),
)
BARS = (bar(0, "100.04", 12_000), bar(1, "100.09", 8_000))
BUY_FILLS = (
    fill("B1", 400, "100.05", 5, fee="1.20"),
    fill("B2", 600, "100.10", 80, fee="1.80"),
)
SELL_FILLS = (
    fill("S1", 400, "100.01", 5, fee="1.20"),
    fill("S2", 600, "100.06", 80, fee="1.80"),
)


def one(side="buy", fills=BUY_FILLS, quotes=QUOTES, bars=BARS, qty=1000, **kwargs):
    analysis = run([order(side=side, qty=qty, decision=-2)], fills, quotes, bars, **kwargs)
    return analysis.results[0]


# -- the worked example -------------------------------------------------------------------


def test_worked_buy_reconciles_to_hand_calculation():
    r = one("buy")
    assert r.filled_qty == 1000 and r.fill_rate == 1
    assert r.executed_notional == D("100080.00")  # 400 * 100.05 + 600 * 100.10
    assert r.avg_price == D("100.08")
    assert r.benchmarks[ARRIVAL_MID].value == D("100.02")

    arrival = r.slippage[ARRIVAL_MID]
    assert arrival.cost == D("60.00")  # 1000 * (100.08 - 100.02)
    assert arrival.notional == D("100020.00")
    assert arrival.bps == D("60") / D("100020") * 10_000
    assert round(arrival.bps, 4) == D("5.9988")

    assert r.spread_cost == D("20.00")  # 400 * 0.02 + 600 * 0.02
    assert r.drift_cost == D("40.00")  # 400 * 0.01 + 600 * 0.06
    assert r.fees == D("3.00")
    assert r.total_cost == D("63.00")
    assert round(r.fee_bps, 4) == D("0.2999")
    assert round(r.effective_spread_bps, 4) == D("3.9976")  # 2 * 20 / 100,060
    assert round(r.arrival_quoted_spread_bps, 4) == D("3.9992")  # 0.04 / 100.02
    assert r.flags == () and r.notes == () and r.trusted and r.decomposition_trusted


def test_worked_sell_in_the_same_market_flips_drift_but_not_spread():
    r = one("sell", SELL_FILLS)
    assert r.avg_price == D("100.04")
    # The market rose after arrival: bad for the buyer above, good for this seller.
    assert r.slippage[ARRIVAL_MID].cost == D("-20.00")
    assert r.drift_cost == D("-40.00")
    # Both crossed the same half spread to trade.
    assert r.spread_cost == D("20.00")
    assert r.total_cost == D("-17.00")
    assert round(r.slippage[ARRIVAL_MID].bps, 4) == D("-1.9996")


def test_decision_and_vwap_benchmarks_for_the_worked_buy():
    r = one("buy")
    assert r.benchmarks[DECISION_MID].value == D("100.00")
    assert r.slippage[DECISION_MID].cost == D("80.00")
    assert r.slippage[DECISION_MID].bps == D("8")
    assert r.benchmarks[INTERVAL_VWAP].value == D("100.06")
    assert r.slippage[INTERVAL_VWAP].cost == D("20.00")
    assert round(r.slippage[INTERVAL_VWAP].bps, 4) == D("1.9988")
    assert r.participation_rate == D("0.05")
    assert all(s.trusted for s in r.slippage.values())


def test_fills_carry_the_quote_they_were_measured_against():
    first, second = one("buy").fills
    assert (first.bid, first.ask, first.mid) == (D("100.01"), D("100.05"), D("100.03"))
    assert first.quote_age_us == 1_000_000 and first.quote_status == OK
    assert (first.spread_cost, first.drift_cost) == (D("8.00"), D("4.00"))
    assert (second.spread_cost, second.drift_cost) == (D("12.00"), D("36.00"))


def test_time_to_fill():
    r = one("buy")
    assert r.time_to_first_fill_us == 5_000_000
    assert r.time_to_last_fill_us == 80_000_000
    assert (r.first_fill_ts, r.last_fill_ts) == (at(5), at(80))


# -- sign convention ----------------------------------------------------------------------


@pytest.mark.parametrize(
    ("side", "price", "expected_cost"),
    [
        ("buy", "100.08", "60.00"),  # bought above arrival mid: unfavourable
        ("sell", "99.96", "60.00"),  # sold below arrival mid: unfavourable
        ("buy", "99.96", "-60.00"),  # bought below: favourable
        ("sell", "100.08", "-60.00"),  # sold above: favourable
    ],
)
def test_cost_is_positive_exactly_when_the_fill_is_unfavourable(side, price, expected_cost):
    r = one(side, [fill("F1", 1000, price, 5)])
    cost = r.slippage[ARRIVAL_MID].cost
    assert cost == D(expected_cost)
    assert (r.slippage[ARRIVAL_MID].bps > 0) == (cost > 0)


def test_mirrored_orders_have_identical_costs():
    """Reflect every price around a pivot and swap the side: all costs must be unchanged."""
    rng = random.Random(11)
    pivot = D("200")
    for _ in range(50):
        bid = D(rng.randrange(9_900, 10_100)) / 100
        ask = bid + D(rng.randrange(1, 6)) / 100
        quotes = [quote(bid, ask, -1), quote(bid + D("0.03"), ask + D("0.03"), 3)]
        fills = [
            fill(f"F{i}", rng.randrange(1, 9) * 100, bid + D(rng.randrange(-5, 12)) / 100, 4 + i)
            for i in range(rng.randrange(1, 4))
        ]
        mirrored_quotes = [
            quote(pivot - q.ask, pivot - q.bid, (q.ts - at(0)) / 1_000_000) for q in quotes
        ]
        mirrored_fills = [
            fill(f.fill_id, f.qty, pivot - f.price, (f.ts - at(0)) / 1_000_000) for f in fills
        ]
        buy = one("buy", fills, quotes, bars=())
        sell = one("sell", mirrored_fills, mirrored_quotes, bars=())
        assert buy.slippage[ARRIVAL_MID].cost == sell.slippage[ARRIVAL_MID].cost
        assert buy.spread_cost == sell.spread_cost
        assert buy.drift_cost == sell.drift_cost


def test_spread_plus_drift_equals_shortfall_exactly():
    rng = random.Random(5)
    for _ in range(50):
        side = rng.choice(["buy", "sell"])
        fills = [
            fill(f"F{i}", rng.randrange(1, 20) * 100, D(rng.randrange(9_990, 10_020)) / 100, 5 + i)
            for i in range(rng.randrange(1, 5))
        ]
        r = one(side, fills, qty=100_000)
        assert r.spread_cost + r.drift_cost == r.slippage[ARRIVAL_MID].cost
        assert r.slippage[ARRIVAL_MID].cost + r.fees == r.total_cost


def test_side_sign():
    assert Side.BUY.sign == 1 and Side.SELL.sign == -1


# -- fees ---------------------------------------------------------------------------------


def test_rebates_reduce_all_in_cost_and_may_make_it_negative():
    fills = [fill("F1", 1000, "100.02", 5, fee="-2.50")]  # filled at arrival mid, paid a rebate
    r = one("buy", fills)
    assert r.slippage[ARRIVAL_MID].cost == 0
    assert r.fees == D("-2.50") and r.total_cost == D("-2.50")
    assert r.fee_bps == D("-2.50") / D("100020") * BPS


# -- fill states --------------------------------------------------------------------------


def test_unfilled_order_has_no_costs_and_is_never_trusted():
    r = one("buy", fills=())
    assert r.flags == ("NO_FILLS",)
    assert r.filled_qty == 0 and r.fill_rate == 0 and r.avg_price is None
    assert r.benchmarks[ARRIVAL_MID].value == D("100.02")  # the benchmark itself still exists
    assert all(s.cost is None and s.bps is None and not s.trusted for s in r.slippage.values())
    assert r.benchmarks[INTERVAL_VWAP].status == MISSING
    assert r.spread_cost is None and r.total_cost is None and r.effective_spread_bps is None
    assert r.time_to_first_fill_us is None and r.participation_rate is None


def test_partial_fill_costs_cover_only_the_executed_quantity():
    r = one("buy", BUY_FILLS[:1])
    assert r.flags == ("PARTIAL_FILL", "VWAP_BARS_COARSE")
    assert r.fill_rate == D("0.4")
    assert r.slippage[ARRIVAL_MID].cost == D("12.00")  # 400 * (100.05 - 100.02)
    assert r.slippage[ARRIVAL_MID].notional == D("40008.00")
    assert r.trusted
    assert any("Filled 400 of 1000" in note for note in r.notes)


def test_overfilled_order_is_computed_but_not_trusted():
    r = one("buy", BUY_FILLS, qty=900)
    assert "OVERFILLED" in r.flags
    assert r.slippage[ARRIVAL_MID].cost == D("60.00")
    assert not r.trusted and not r.decomposition_trusted
    assert not any(s.trusted for s in r.slippage.values())


def test_a_fill_stamped_before_arrival_marks_the_order_untrusted():
    r = one("buy", [fill("F1", 1000, "100.05", -0.2)])
    assert "FILL_BEFORE_ARRIVAL" in r.flags and "VWAP_WINDOW_INVALID" in r.flags
    assert r.slippage[ARRIVAL_MID].cost is not None and not r.trusted
    assert r.time_to_first_fill_us == -200_000


def test_fills_are_ordered_by_time_whatever_order_they_arrive_in():
    r = one("buy", tuple(reversed(BUY_FILLS)))
    assert [f.fill.fill_id for f in r.fills] == ["B1", "B2"]


# -- benchmark quality --------------------------------------------------------------------


def test_missing_arrival_quote_is_flagged_and_never_backfilled_from_later_quotes():
    later_only = [q for q in QUOTES if q.ts > at(0)]
    r = one("buy", quotes=later_only)
    arrival = r.benchmarks[ARRIVAL_MID]
    assert arrival.value is None and arrival.status == MISSING
    assert "ARRIVAL_QUOTE_MISSING" in r.flags and "DECISION_QUOTE_MISSING" in r.flags
    assert r.slippage[ARRIVAL_MID].cost is None and r.slippage[ARRIVAL_MID].bps is None
    assert not r.trusted
    assert r.total_cost is None and r.fee_bps is None
    assert r.fees == D("3.00")  # explicit fees need no benchmark
    # The split needs an arrival price, but each fill is still measured against its own quote.
    assert r.spread_cost is None and r.drift_cost is None
    assert [f.spread_cost for f in r.fills] == [D("8.00"), D("12.00")]
    assert round(r.effective_spread_bps, 4) == D("3.9976")
    assert any("never used to backfill" in note for note in r.notes)


def test_stale_arrival_quote_gives_a_value_that_is_not_trusted():
    quotes = [quote("100.00", "100.04", -30), *QUOTES[3:]]
    r = one("buy", quotes=quotes)
    arrival = r.benchmarks[ARRIVAL_MID]
    assert arrival.value == D("100.02") and arrival.status == DEGRADED
    assert "ARRIVAL_QUOTE_STALE" in r.flags
    assert r.slippage[ARRIVAL_MID].cost == D("60.00")
    assert not r.trusted and not r.decomposition_trusted
    assert any("30.0s old (limit 5s)" in note for note in r.notes)


def test_a_fill_without_a_prior_quote_blocks_the_split_but_not_the_shortfall():
    quotes = [q for q in QUOTES if q.ts < at(0)]  # feed dies right after arrival
    fills = [fill("F1", 1000, "100.08", 2)]  # quote is 3s old: still usable
    fresh = one("buy", fills, quotes)
    assert fresh.spread_cost is not None and fresh.decomposition_trusted

    late = one("buy", BUY_FILLS, quotes)  # fills at +5s and +80s, last quote at -0.5s
    assert "FILL_QUOTE_STALE" in late.flags
    assert late.trusted  # arrival shortfall is unaffected
    assert late.spread_cost is not None and not late.decomposition_trusted

    no_quotes_before_fill = one(
        "buy", [fill("F1", 1000, "100.08", 2, order_id="O1")], quotes=[quote("100", "100.04", 3)]
    )
    assert "FILL_QUOTE_MISSING" in no_quotes_before_fill.flags
    assert no_quotes_before_fill.fills[0].quote_status == MISSING
    assert no_quotes_before_fill.spread_cost is None


def test_a_quote_stamped_at_the_fill_instant_is_not_used_for_that_fill():
    quotes = [quote("100.00", "100.04", -1), quote("100.10", "100.14", 5)]
    r = one("buy", [fill("F1", 1000, "100.04", 5)], quotes)
    assert r.fills[0].mid == D("100.02")  # the quote in force before the trade


def test_vwap_problems_do_not_taint_the_arrival_benchmark():
    r = one("buy", bars=BARS[:1])
    assert "VWAP_BARS_PARTIAL" in r.flags
    assert r.trusted and not r.slippage[INTERVAL_VWAP].trusted
    assert r.slippage[INTERVAL_VWAP].cost == D("40.00")  # 1000 * (100.08 - 100.04)


def test_methodology_thresholds_change_trust_not_values():
    from tca.benchmark import Methodology

    quotes = [quote("100.00", "100.04", -3), *QUOTES[3:]]
    default = one("buy", quotes=quotes)
    strict = one("buy", quotes=quotes, methodology=Methodology(max_quote_age_ms=1000))
    assert default.trusted and not strict.trusted
    assert default.slippage[ARRIVAL_MID].cost == strict.slippage[ARRIVAL_MID].cost


# -- analysis-level -----------------------------------------------------------------------


def test_results_are_ordered_by_arrival_and_orphan_fills_are_counted():
    analysis = run(
        [order("LATE", arrival=10), order("EARLY", arrival=1)],
        [
            fill("F1", 100, "100.05", 12, order_id="LATE"),
            fill("F2", 100, "100.05", 12, order_id="GHOST"),
            fill("F3", 100, "100.05", 13, order_id="GHOST"),
        ],
        QUOTES,
    )
    assert [r.order.order_id for r in analysis.results] == ["EARLY", "LATE"]
    assert analysis.orphan_fills == 2
    assert analysis.by_id["LATE"].filled_qty == 100


def test_child_orders_are_attached_and_venues_listed():
    analysis = run(
        [order()],
        [fill("F1", 100, "100.05", 5, venue="V2", child_id="C2")],
        QUOTES,
        child_orders=[child("C2", "V2", 100, 4), child("C1", "V1", 100, 3)],
    )
    result = analysis.results[0]
    assert [c.child_id for c in result.child_orders] == ["C1", "C2"]
    assert result.venues == ("V1", "V2")
