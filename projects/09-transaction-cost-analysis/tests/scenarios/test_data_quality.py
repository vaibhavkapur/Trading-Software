"""Failure scenarios from the blueprint's test strategy, driven through the service and API:
missing data, clock skew, stale events, duplicate delivery, rejected writes, and
connection loss followed by recovery.

Each scenario asserts two things: the problem is visible (a flag, a note, a reject), and
it cannot quietly leak into a trusted aggregate.
"""

from __future__ import annotations

import random
from decimal import Decimal

import pytest
from builders import FIXTURE_DIR, WORKED_DIR
from fastapi.testclient import TestClient

from tca.api import create_app
from tca.benchmark import Methodology
from tca.report import build_bundle
from tca.service import TcaService
from tca.store import Store

ORDERS = "order_id,symbol,side,qty,decision_ts,arrival_ts,broker,strategy\n"
FILLS = "fill_id,order_id,child_id,venue,qty,price,fee,ts\n"
QUOTES = "symbol,venue,bid,ask,ts,recv_ts\n"
BARS = "symbol,ts,interval_s,vwap,volume\n"

DAY = "2026-03-02T"
# A buy for 100 that arrives at 15:00:30 and fills completely at 15:00:35.
ORDER = ORDERS + f"O1,XYZ,buy,100,{DAY}15:00:29Z,{DAY}15:00:30Z,B,S\n"
FILL = FILLS + f"F1,O1,,V1,100,100.10,0.50,{DAY}15:00:35Z\n"


def load(service: TcaService, **datasets: str):
    results = {name: service.import_csv(name, text) for name, text in datasets.items()}
    for result in results.values():
        assert result.rejected == 0, result.rejects
    return results


def costed(service: TcaService, **options) -> int:
    return service.summary("all", **options)["totals"]["orders_costed"]


# -- connection loss and recovery -----------------------------------------------------------


def test_a_quote_feed_outage_is_flagged_and_heals_when_the_gap_is_filled():
    """The feed drops at 14:59:50 and resumes at 15:00:40. The order lives inside the gap."""
    service = TcaService()
    load(
        service,
        orders=ORDER,
        fills=FILL,
        quotes=QUOTES
        + f"XYZ,V1,99.98,100.02,{DAY}14:59:50Z,\n"  # last quote before the outage
        + f"XYZ,V1,100.20,100.24,{DAY}15:00:40Z,\n",  # first quote after it
    )
    outage = service.analysis()
    result = outage.by_id["O1"]
    arrival = result.benchmarks["arrival_mid"]

    # The pre-outage quote is used, labelled with its age. The post-outage quote is not.
    assert arrival.value is not None and arrival.status == "degraded"
    assert str(arrival.value) == "100.00"
    assert arrival.details["quote_age_us"] == 40_000_000
    assert {"ARRIVAL_QUOTE_STALE", "DECISION_QUOTE_STALE", "FILL_QUOTE_STALE"} <= set(result.flags)
    assert any("40.0s old" in note for note in result.notes)
    assert not result.trusted
    assert costed(service) == 0 and costed(service, include_untrusted=True) == 1

    # Recovery: the quotes captured during the outage are delivered late.
    load(
        service,
        quotes=QUOTES
        + f"XYZ,V1,100.04,100.08,{DAY}15:00:28Z,\n"
        + f"XYZ,V1,100.06,100.10,{DAY}15:00:33Z,\n",
    )
    healed = service.analysis()
    result = healed.by_id["O1"]
    assert healed.dataset.version != outage.dataset.version
    assert result.flags == ("VWAP_BARS_MISSING",)  # no bars were loaded; quotes are clean
    assert result.trusted and result.decomposition_trusted
    assert str(result.benchmarks["arrival_mid"].value) == "100.06"
    assert str(result.slippage["arrival_mid"].cost) == "4.00"
    assert costed(service) == 1


def test_one_venue_dropping_out_does_not_freeze_its_last_quote_into_the_benchmark():
    """V2 stops quoting with the best bid on record. Six seconds later it has aged out."""
    service = TcaService()
    load(
        service,
        orders=ORDER,
        fills=FILL,
        quotes=QUOTES
        + f"XYZ,V2,100.05,100.07,{DAY}15:00:24Z,\n"  # V2's last quote: 6s before arrival
        + f"XYZ,V1,100.00,100.04,{DAY}15:00:29Z,\n",
    )
    result = service.analysis().by_id["O1"]
    arrival = result.benchmarks["arrival_mid"]
    assert str(arrival.value) == "100.02" and arrival.status == "ok"
    assert arrival.details["bid_venue"] == arrival.details["ask_venue"] == "V1"
    assert result.trusted


# -- stale events and missing data ----------------------------------------------------------


def test_staleness_is_a_methodology_parameter_and_changes_trust_not_the_number():
    quotes = QUOTES + f"XYZ,V1,100.00,100.04,{DAY}15:00:22Z,\n"  # 8s old at arrival
    values = {}
    for limit in (5_000, 10_000):
        service = TcaService(methodology=Methodology(max_quote_age_ms=limit))
        load(service, orders=ORDER, fills=FILL, quotes=quotes)
        result = service.analysis().by_id["O1"]
        values[limit] = (str(result.slippage["arrival_mid"].cost), result.trusted)
    assert values == {5_000: ("8.00", False), 10_000: ("8.00", True)}


def test_with_no_market_data_at_all_only_benchmark_free_measures_are_reported():
    service = TcaService()
    load(service, orders=ORDER, fills=FILL)
    result = service.analysis().by_id["O1"]
    assert {"ARRIVAL_QUOTE_MISSING", "FILL_QUOTE_MISSING", "VWAP_BARS_MISSING"} <= set(result.flags)
    assert all(slippage.cost is None for slippage in result.slippage.values())
    totals = service.summary("all", include_untrusted=True)["totals"]
    assert totals["slippage_bps"] is None and totals["effective_spread_bps"] is None
    assert (totals["fill_rate"], totals["fees"]) == ("1.000000", "0.5000")
    assert totals["avg_time_to_first_fill_ms"] == "5000.000"


def test_missing_bars_degrade_vwap_without_touching_the_arrival_benchmark():
    service = TcaService()
    load(
        service,
        orders=ORDER,
        quotes=QUOTES + f"XYZ,V1,100.00,100.04,{DAY}15:00:29Z,\n",
        fills=FILLS
        + f"F1,O1,,V1,50,100.10,0,{DAY}15:00:35Z\n"
        + f"F2,O1,,V1,50,100.10,0,{DAY}15:02:10Z\n",
        bars=BARS
        + f"XYZ,{DAY}15:00:00Z,60,100.05,1000\n"  # the 15:01 bar never arrived
        + f"XYZ,{DAY}15:02:00Z,60,100.15,3000\n",
    )
    result = service.analysis().by_id["O1"]
    vwap = result.benchmarks["interval_vwap"]
    assert "VWAP_BARS_PARTIAL" in result.flags
    assert (vwap.details["bars_found"], vwap.details["bars_expected"]) == (2, 3)
    assert not result.slippage["interval_vwap"].trusted
    assert result.slippage["arrival_mid"].trusted
    assert costed(service, benchmark="interval_vwap") == 0
    assert costed(service, benchmark="arrival_mid") == 1


# -- clock skew -----------------------------------------------------------------------------


SKEWED = (
    QUOTES
    + f"XYZ,V1,100.00,100.04,{DAY}15:00:27Z,{DAY}15:00:27.010Z\n"
    # Stamped by the venue at 15:00:29.5 but only received at 15:00:31.5, after arrival.
    + f"XYZ,V1,100.10,100.14,{DAY}15:00:29.500Z,{DAY}15:00:31.500Z\n"
)


def test_a_quote_whose_clocks_disagree_is_used_but_not_trusted():
    service = TcaService()
    load(service, orders=ORDER, fills=FILL, quotes=SKEWED)
    result = service.analysis().by_id["O1"]
    arrival = result.benchmarks["arrival_mid"]
    assert str(arrival.value) == "100.12" and arrival.status == "degraded"
    assert "ARRIVAL_CLOCK_SKEW" in result.flags
    assert arrival.details["clock_skew_us"] == 2_000_000
    assert any("2000ms apart" in note for note in result.notes)
    assert not result.trusted and costed(service) == 0


def test_the_receive_clock_shows_only_what_had_been_received_by_arrival():
    """Under quote_clock=receive the late quote is in the future at arrival, so the
    benchmark is the earlier quote and the answer differs: the clock choice matters."""
    service = TcaService(methodology=Methodology(quote_clock="receive"))
    load(service, orders=ORDER, fills=FILL, quotes=SKEWED)
    result = service.analysis().by_id["O1"]
    arrival = result.benchmarks["arrival_mid"]
    assert str(arrival.value) == "100.02"
    assert "ARRIVAL_CLOCK_SKEW" not in result.flags
    assert service.analysis().methodology.id != Methodology().id


def test_a_fill_stamped_before_its_order_arrived_is_never_trusted():
    service = TcaService()
    load(
        service,
        orders=ORDER,
        quotes=QUOTES + f"XYZ,V1,100.00,100.04,{DAY}15:00:25Z,\n",
        fills=FILLS + f"F1,O1,,V1,100,100.10,0,{DAY}15:00:29.900Z\n",  # 100ms before arrival
    )
    result = service.analysis().by_id["O1"]
    assert "FILL_BEFORE_ARRIVAL" in result.flags
    assert result.benchmarks["arrival_mid"].status == "ok"  # the quote itself is fine
    assert not any(slippage.trusted for slippage in result.slippage.values())
    assert costed(service) == 0


# -- duplicate delivery ---------------------------------------------------------------------


def test_redelivering_everything_changes_nothing():
    service = TcaService()
    service.import_directory(FIXTURE_DIR)
    first = service.analysis()
    before = build_bundle(first)

    again = service.import_directory(FIXTURE_DIR)
    assert sum(result.accepted for result in again) == 0
    assert sum(result.rejected for result in again) == 0
    assert sum(result.duplicates for result in again) == sum(first.dataset.counts.values())
    assert service.analysis() is first  # no new revision, so the cached analysis is reused
    assert build_bundle(service.analysis()) == before


def test_a_fill_delivered_twice_is_counted_once():
    service = TcaService()
    load(service, orders=ORDER, quotes=QUOTES + f"XYZ,V1,100.00,100.04,{DAY}15:00:29Z,\n")
    load(service, fills=FILL)
    result = load(service, fills=FILL + f"F2,O1,,V1,0100,100.100,0.5,{DAY}15:00:35Z\n")
    # F1 is a duplicate; F2 is a genuinely different fill that overfills the order.
    assert (result["fills"].accepted, result["fills"].duplicates) == (1, 1)
    order = service.analysis().by_id["O1"]
    assert order.filled_qty == 200 and "OVERFILLED" in order.flags
    assert not order.trusted and costed(service) == 0
    assert any("duplicated or misattributed" in note for note in order.notes)


# -- rejected writes ------------------------------------------------------------------------


def test_a_restated_record_is_rejected_and_the_original_kept():
    service = TcaService()
    load(
        service,
        orders=ORDER,
        fills=FILL,
        quotes=QUOTES + f"XYZ,V1,100.00,100.04,{DAY}15:00:29Z,\n",
    )
    before = service.analysis()
    restated = service.import_csv("fills", FILLS + f"F1,O1,,V1,100,99.00,0.50,{DAY}15:00:35Z\n")
    assert (restated.accepted, restated.duplicates, restated.rejected) == (0, 0, 1)
    assert restated.rejects[0].code == "CONFLICT"
    after = service.analysis()
    assert after.dataset.version == before.dataset.version
    assert after.by_id["O1"].avg_price == Decimal("100.10")


def test_bad_rows_are_rejected_individually_and_reported_with_reasons():
    client = TestClient(create_app())
    body = (
        ORDERS
        + f"GOOD,XYZ,buy,100,{DAY}15:00:29Z,{DAY}15:00:30Z,B,S\n"
        + f"NAIVE,XYZ,buy,100,2026-03-02T15:00:29,{DAY}15:00:30Z,B,S\n"  # no UTC offset
        + f"SIDE,XYZ,short,100,{DAY}15:00:29Z,{DAY}15:00:30Z,B,S\n"
        + f"QTY,XYZ,buy,0,{DAY}15:00:29Z,{DAY}15:00:30Z,B,S\n"
        + f"ORDER,XYZ,buy,100,{DAY}15:00:31Z,{DAY}15:00:30Z,B,S\n"  # decided after arrival
    )
    response = client.post("/v1/imports/orders", content=body, headers={"content-type": "text/csv"})
    result = response.json()
    assert response.status_code == 200
    assert (result["rows"], result["accepted"], result["rejected"]) == (5, 1, 4)
    assert [(r["row"], r["code"]) for r in result["rejects"]] == [
        (2, "BAD_TIMESTAMP"),
        (3, "BAD_SIDE"),
        (4, "OUT_OF_RANGE"),
        (5, "DECISION_AFTER_ARRIVAL"),
    ]
    orders = client.get("/v1/tca/orders").json()["orders"]
    assert [order["order_id"] for order in orders] == ["GOOD"]
    assert 'tca_import_rows_total{dataset="orders",outcome="rejected"} 4.0' in (
        client.get("/metrics").text
    )


def test_a_payload_with_the_wrong_columns_is_refused_whole():
    client = TestClient(create_app())
    before = client.get("/v1/datasets").json()["version"]
    response = client.post(
        "/v1/imports/fills",
        content="fill_id,order_id,qty,px\nF1,O1,100,100.10\n",
        headers={"content-type": "text/csv"},
    )
    assert response.status_code == 400
    after = client.get("/v1/datasets").json()
    assert after["version"] == before and after["rows"]["fills"] == 0


def test_a_crossed_quote_row_is_rejected_at_import():
    service = TcaService()
    result = service.import_csv("quotes", QUOTES + f"XYZ,V1,100.05,100.04,{DAY}15:00:29Z,\n")
    assert result.rejects[0].code == "CROSSED_QUOTE"
    assert service.analysis().dataset.counts["quotes"] == 0


def test_venues_crossing_each_other_are_kept_and_flagged():
    """Each venue's own quote is valid, but V1's bid is above V2's ask."""
    service = TcaService()
    load(
        service,
        orders=ORDER,
        fills=FILL,
        quotes=QUOTES
        + f"XYZ,V1,100.06,100.08,{DAY}15:00:29Z,\n"
        + f"XYZ,V2,100.00,100.04,{DAY}15:00:29Z,\n",
    )
    result = service.analysis().by_id["O1"]
    assert "ARRIVAL_QUOTE_CROSSED" in result.flags
    assert result.benchmarks["arrival_mid"].value is not None and not result.trusted
    assert costed(service) == 0


# -- out-of-order delivery ------------------------------------------------------------------


def test_fills_that_arrive_before_their_orders_wait_as_orphans():
    service = TcaService()
    service.import_file("fills", WORKED_DIR / "fills.csv")
    early = service.analysis()
    assert early.results == () and early.orphan_fills == 4
    assert service.datasets()["orphan_fills"] == 4

    for name in ("quotes", "bars", "orders"):
        service.import_file(name, WORKED_DIR / f"{name}.csv")
    late = service.analysis()
    assert late.orphan_fills == 0

    in_order = TcaService()
    in_order.import_directory(WORKED_DIR)
    assert build_bundle(late) == build_bundle(in_order.analysis())


@pytest.mark.parametrize("seed", [1, 2, 3])
def test_row_order_and_batching_do_not_change_the_dataset_version_or_the_report(seed):
    reference = TcaService()
    reference.import_directory(FIXTURE_DIR)
    expected = build_bundle(reference.analysis())

    rng = random.Random(seed)
    shuffled = TcaService()
    names = ["orders", "child_orders", "fills", "quotes", "bars"]
    rng.shuffle(names)
    for name in names:
        header, *lines = (FIXTURE_DIR / f"{name}.csv").read_text().splitlines()
        rng.shuffle(lines)
        cut = rng.randrange(1, len(lines))
        for chunk in (lines[:cut], lines[cut:]):
            result = shuffled.import_csv(name, "\n".join([header, *chunk]) + "\n")
            assert result.rejected == 0
    assert build_bundle(shuffled.analysis()) == expected


def test_results_survive_a_restart_from_the_database_file(tmp_path):
    path = str(tmp_path / "tca.sqlite3")
    first = TcaService(Store(path))
    first.import_directory(WORKED_DIR)
    expected = build_bundle(first.analysis())
    first.store.close()

    restarted = TcaService(Store(path))
    assert build_bundle(restarted.analysis()) == expected
