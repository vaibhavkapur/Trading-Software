"""The four definition-of-done items from the blueprint, each checked end to end.

1. Signed shortfall is positive for unfavourable fills, for buys and sells.
2. Missing arrival quotes are flagged and never backfilled from future data.
3. Report output includes the benchmark methodology and dataset versions.
4. Sample calculations reconcile manually within rounding tolerance.

Item 4 uses a second, deliberately naive implementation written against the raw CSV files
with ``fractions.Fraction``. It shares no code with ``tca`` beyond the file format.
"""

from __future__ import annotations

import csv
import io
import json
from collections import defaultdict
from datetime import UTC, datetime, timedelta
from decimal import Decimal
from fractions import Fraction

import pytest
from builders import FIXTURE_DIR, WORKED_DIR
from fastapi.testclient import TestClient

from tca.__main__ import main
from tca.api import create_app
from tca.benchmark import BENCHMARKS, Methodology
from tca.report import build_bundle
from tca.service import TcaService

ORDERS = "order_id,symbol,side,qty,decision_ts,arrival_ts,broker,strategy\n"
FILLS = "fill_id,order_id,child_id,venue,qty,price,fee,ts\n"
QUOTES = "symbol,venue,bid,ask,ts,recv_ts\n"
BARS = "symbol,ts,interval_s,vwap,volume\n"

# Half a unit in the last exported decimal place (4 dp for money and bps).
TOLERANCE = Fraction(1, 20_000)


def load(service: TcaService, **datasets: str) -> None:
    for name, text in datasets.items():
        result = service.import_csv(name, text)
        assert result.rejected == 0, result.rejects


def rows_of(text: str) -> list[dict[str, str]]:
    return list(csv.DictReader(io.StringIO(text)))


# -- 1. sign convention ---------------------------------------------------------------------


@pytest.fixture
def two_sided() -> TcaService:
    """A buy and a sell in the same market (mid 100.00, VWAP 100.00), each filled at a
    price that is bad for it and at a price that is good for it."""
    service = TcaService()
    load(
        service,
        quotes=QUOTES + "XYZ,V1,99.98,100.02,2026-03-02T15:00:00Z,\n",
        bars=BARS + "XYZ,2026-03-02T15:00:00Z,60,100.00,10000\n",
        orders=ORDERS
        + "".join(
            f"{order_id},XYZ,{side},100,2026-03-02T15:00:01Z,2026-03-02T15:00:02Z,B,S\n"
            for order_id, side in [
                ("BUY-BAD", "buy"),
                ("BUY-GOOD", "buy"),
                ("SELL-BAD", "sell"),
                ("SELL-GOOD", "sell"),
            ]
        ),
        fills=FILLS
        + "F1,BUY-BAD,,V1,100,100.10,0,2026-03-02T15:00:10Z\n"  # paid above the mid
        + "F2,BUY-GOOD,,V1,100,99.90,0,2026-03-02T15:00:10Z\n"  # paid below it
        + "F3,SELL-BAD,,V1,100,99.90,0,2026-03-02T15:00:10Z\n"  # received below the mid
        + "F4,SELL-GOOD,,V1,100,100.10,0,2026-03-02T15:00:10Z\n",  # received above it
    )
    return service


@pytest.mark.parametrize("benchmark", BENCHMARKS)
def test_unfavourable_fills_cost_positive_for_buys_and_sells(two_sided, benchmark):
    by_id = two_sided.analysis().by_id
    for order_id in ("BUY-BAD", "SELL-BAD"):
        slippage = by_id[order_id].slippage[benchmark]
        assert slippage.cost == Decimal("10.00") and slippage.bps > 0, order_id
        assert slippage.trusted or benchmark == "interval_vwap"  # one bar: flagged coarse
    for order_id in ("BUY-GOOD", "SELL-GOOD"):
        slippage = by_id[order_id].slippage[benchmark]
        assert slippage.cost == Decimal("-10.00") and slippage.bps < 0, order_id


def test_buy_and_sell_costs_are_never_netted_against_each_other():
    """A bad buy and a bad sell sum to twice the cost; they do not cancel to zero."""
    bad = TcaService()
    load(
        bad,
        quotes=QUOTES + "XYZ,V1,99.98,100.02,2026-03-02T15:00:00Z,\n",
        orders=ORDERS
        + "B,XYZ,buy,100,2026-03-02T15:00:01Z,2026-03-02T15:00:02Z,B,S\n"
        + "S,XYZ,sell,100,2026-03-02T15:00:01Z,2026-03-02T15:00:02Z,B,S\n",
        fills=FILLS
        + "F1,B,,V1,100,100.10,0,2026-03-02T15:00:10Z\n"
        + "F2,S,,V1,100,99.90,0,2026-03-02T15:00:10Z\n",
    )
    totals = bad.summary("all")["totals"]
    assert totals["slippage_cost"] == "20.0000"
    assert totals["slippage_bps"] == "10.0000"
    by_side = {row["key"]: row["slippage_bps"] for row in bad.summary("side")["rows"]}
    assert by_side == {"buy": "10.0000", "sell": "10.0000"}


def test_fixture_signs_agree_with_an_independent_reading_of_the_raw_files(reference, report):
    """For every costed order: the exported sign matches 'did it pay up / sell down'."""
    checked = {"buy": 0, "sell": 0}
    for row in rows_of(report["orders.csv"]):
        ref = reference[row["order_id"]]
        if ref["mid"] is None or not ref["filled"]:
            continue
        average = ref["notional"] / ref["filled"]
        worse = average > ref["mid"] if row["side"] == "buy" else average < ref["mid"]
        better = average < ref["mid"] if row["side"] == "buy" else average > ref["mid"]
        cost = Decimal(row["arrival_mid_cost"])
        if worse:
            assert cost > 0, row["order_id"]
        if better:
            assert cost < 0, row["order_id"]
        checked[row["side"]] += 1
    assert checked["buy"] >= 10 and checked["sell"] >= 10


# -- 2. missing arrival quotes --------------------------------------------------------------


def late_feed() -> TcaService:
    """The order arrives at 15:00:00 and the first quote is not until 15:00:01."""
    service = TcaService()
    load(
        service,
        orders=ORDERS + "O1,XYZ,buy,100,2026-03-02T14:59:59Z,2026-03-02T15:00:00Z,B,S\n",
        fills=FILLS + "F1,O1,,V1,100,100.10,0.50,2026-03-02T15:00:05Z\n",
        quotes=QUOTES + "XYZ,V1,100.00,100.04,2026-03-02T15:00:01Z,\n",
    )
    return service


def test_a_missing_arrival_quote_is_flagged_not_backfilled():
    service = late_feed()
    result = service.analysis().by_id["O1"]
    arrival = result.benchmarks["arrival_mid"]
    assert arrival.value is None and arrival.status == "missing"
    assert "ARRIVAL_QUOTE_MISSING" in result.flags
    assert result.slippage["arrival_mid"].cost is None
    assert not result.trusted
    assert any("never used to backfill" in note for note in result.notes)
    # The later quote is still used where it is legitimately in the past: at the fill.
    assert result.fills[0].mid == Decimal("100.02")


def test_more_future_quotes_never_change_a_missing_arrival_benchmark():
    service = late_feed()
    before = service.analysis().by_id["O1"]
    load(
        service,
        quotes=QUOTES
        + "XYZ,V1,100.01,100.05,2026-03-02T15:00:00.000001Z,\n"  # one microsecond late
        + "XYZ,V2,99.99,100.03,2026-03-02T15:00:02Z,\n",
    )
    after = service.analysis().by_id["O1"]
    assert after.benchmarks["arrival_mid"] == before.benchmarks["arrival_mid"]
    assert after.slippage["arrival_mid"] == before.slippage["arrival_mid"]


def test_a_missing_benchmark_is_exported_blank_and_left_out_of_aggregates():
    service = late_feed()
    client = TestClient(create_app(service))

    detail = client.get("/v1/tca/orders/O1", params={"benchmark": "arrival_mid"}).json()
    assert detail["headline"] == {
        "benchmark": "arrival_mid",
        "value": None,
        "status": "missing",
        "cost": None,
        "bps": None,
        "trusted": False,
    }
    assert "ARRIVAL_QUOTE_MISSING" in detail["flags"]

    (row,) = rows_of(client.get("/v1/tca/export.csv").text)
    assert row["arrival_mid"] == row["arrival_mid_cost"] == row["arrival_mid_bps"] == ""
    assert row["arrival_mid_status"] == "missing" and row["trusted"] == "false"

    for include_untrusted in (False, True):
        totals = client.get(
            "/v1/tca/summary", params={"group_by": "all", "include_untrusted": include_untrusted}
        ).json()["totals"]
        assert totals["orders_filled"] == 1 and totals["orders_costed"] == 0
        assert totals["slippage_bps"] is None  # not "0.0000": there is nothing to report
        assert totals["fees"] == "0.5000"  # explicit costs need no benchmark


def test_every_fixture_order_without_a_prior_quote_is_flagged(reference, fixture_analysis):
    missing = [order_id for order_id, ref in reference.items() if not ref["prior_quote"]]
    assert missing, "the fixture should contain orders that arrive before any quote"
    for order_id in missing:
        result = fixture_analysis.by_id[order_id]
        assert result.benchmarks["arrival_mid"].value is None
        assert "ARRIVAL_QUOTE_MISSING" in result.flags and not result.trusted
    flagged = {
        result.order.order_id
        for result in fixture_analysis.results
        if "ARRIVAL_QUOTE_MISSING" in result.flags
    }
    assert flagged == set(missing)


# -- 3. versions in every output ------------------------------------------------------------


def test_report_bundle_states_methodology_and_dataset_versions(tmp_path):
    out = tmp_path / "report"
    assert main(["report", "--data", str(FIXTURE_DIR), "--out", str(out)]) == 0
    manifest = json.loads((out / "manifest.json").read_text())

    methodology = manifest["methodology"]
    assert methodology["id"] == Methodology().id
    assert methodology["parameters"] == Methodology().params
    assert "positive when the execution was worse" in methodology["sign_convention"]
    assert set(methodology["benchmarks"]) == set(BENCHMARKS)
    assert methodology["rounding"]["mode"] == "half-even"

    dataset = manifest["dataset"]
    assert dataset["version"].startswith("sha256:")
    assert set(dataset["versions"]) == {"orders", "child_orders", "fills", "quotes", "bars"}
    assert all(version.startswith("sha256:") for version in dataset["versions"].values())

    orders = rows_of((out / "orders.csv").read_text())
    assert {row["methodology_version"] for row in orders} == {methodology["id"]}
    assert {row["dataset_version"] for row in orders} == {dataset["version"]}
    benchmarks = rows_of((out / "benchmarks.csv").read_text())
    assert {row["methodology_version"] for row in benchmarks} == {methodology["id"]}


def test_api_responses_state_methodology_and_dataset_versions():
    service = TcaService()
    service.import_directory(WORKED_DIR)
    client = TestClient(create_app(service))
    methodology_id = client.get("/v1/methodology").json()["id"]
    dataset_version = client.get("/v1/datasets").json()["version"]
    for path in ("/v1/tca/orders/W-BUY", "/v1/tca/summary", "/v1/tca/orders"):
        body = client.get(path).json()
        assert body["methodology_version"] == methodology_id, path
        assert body["dataset_version"] == dataset_version, path
    for row in rows_of(client.get("/v1/tca/export.csv").text):
        assert row["methodology_version"] == methodology_id
        assert row["dataset_version"] == dataset_version


def test_changing_data_or_methodology_changes_the_stated_version():
    service = TcaService()
    service.import_directory(WORKED_DIR)
    base = build_bundle(service.analysis())
    base_manifest = json.loads(base["manifest.json"])

    stricter = TcaService(methodology=Methodology(max_quote_age_ms=1000))
    stricter.import_directory(WORKED_DIR)
    stricter_manifest = json.loads(build_bundle(stricter.analysis())["manifest.json"])
    assert stricter_manifest["dataset"]["version"] == base_manifest["dataset"]["version"]
    assert stricter_manifest["methodology"]["id"] != base_manifest["methodology"]["id"]
    assert stricter_manifest["report_id"] != base_manifest["report_id"]

    load(service, quotes=QUOTES + "XYZ,V1,100.10,100.14,2026-03-02T15:05:00Z,\n")
    grown_manifest = json.loads(build_bundle(service.analysis())["manifest.json"])
    assert grown_manifest["dataset"]["version"] != base_manifest["dataset"]["version"]
    assert (
        grown_manifest["dataset"]["versions"]["fills"]
        == (base_manifest["dataset"]["versions"]["fills"])
    )
    assert grown_manifest["report_id"] != base_manifest["report_id"]


# -- 4. manual reconciliation ---------------------------------------------------------------


def us(text: str) -> int:
    moment = datetime.fromisoformat(text)
    return (moment - datetime(1970, 1, 1, tzinfo=UTC)) // timedelta(microseconds=1)


def read(name: str) -> list[dict[str, str]]:
    with (FIXTURE_DIR / f"{name}.csv").open(newline="") as handle:
        return list(csv.DictReader(handle))


@pytest.fixture(scope="module")
def reference() -> dict[str, dict]:
    """Arrival mid and fill totals for every fixture order, straight from the CSVs.

    Arrival mid: for each venue take its last quote at or before arrival; drop venues whose
    quote is more than five seconds old; the mid is halfway between the highest remaining
    bid and the lowest remaining ask. ``mid`` is None when no venue has a fresh quote, and
    ``prior_quote`` says whether any quote at all precedes arrival.
    """
    quotes = defaultdict(list)
    for row in read("quotes"):
        quotes[row["symbol"]].append(
            (us(row["ts"]), row["venue"], Fraction(row["bid"]), Fraction(row["ask"]))
        )
    fills = defaultdict(list)
    for row in read("fills"):
        fills[row["order_id"]].append(row)

    out = {}
    for order in read("orders"):
        arrival = us(order["arrival_ts"])
        latest: dict[str, tuple] = {}
        for ts, venue, bid, ask in quotes[order["symbol"]]:
            if ts <= arrival and (venue not in latest or ts > latest[venue][0]):
                latest[venue] = (ts, bid, ask)
        fresh = [q for q in latest.values() if arrival - q[0] <= 5_000_000]
        mid = (max(q[1] for q in fresh) + min(q[2] for q in fresh)) / 2 if fresh else None
        own = fills[order["order_id"]]
        out[order["order_id"]] = {
            "side": order["side"],
            "qty": int(order["qty"]),
            "mid": mid,
            "prior_quote": bool(latest),
            "filled": sum(int(f["qty"]) for f in own),
            "notional": sum((int(f["qty"]) * Fraction(f["price"]) for f in own), Fraction(0)),
            "fees": sum((Fraction(f["fee"]) for f in own), Fraction(0)),
        }
    return out


@pytest.fixture(scope="module")
def report(fixture_analysis) -> dict[str, str]:
    return build_bundle(fixture_analysis)


def close(exported: str, expected: Fraction) -> bool:
    return abs(Fraction(exported) - expected) <= TOLERANCE


def test_every_fixture_order_reconciles_with_the_independent_calculation(reference, report):
    reconciled = 0
    for row in rows_of(report["orders.csv"]):
        ref = reference[row["order_id"]]
        assert int(row["filled_qty"]) == ref["filled"]
        assert close(row["fees"], ref["fees"]), row["order_id"]
        assert close(row["fill_rate"], Fraction(ref["filled"], ref["qty"]))
        if ref["mid"] is None:
            # Stale or missing: the engine must not claim a trusted figure.
            assert row["arrival_mid_trusted"] == "false", row["order_id"]
            continue
        assert Fraction(row["arrival_mid"]) == ref["mid"], row["order_id"]
        if not ref["filled"]:
            assert row["arrival_mid_cost"] == ""
            continue
        sign = 1 if ref["side"] == "buy" else -1
        cost = sign * (ref["notional"] - ref["filled"] * ref["mid"])
        bps = cost / (ref["filled"] * ref["mid"]) * 10_000
        assert close(row["arrival_mid_cost"], cost), row["order_id"]
        assert close(row["arrival_mid_bps"], bps), row["order_id"]
        assert close(row["total_cost"], cost + ref["fees"]), row["order_id"]
        reconciled += 1
    assert reconciled >= 45


def test_the_headline_aggregate_reconciles_with_the_independent_calculation(reference, report):
    """Sum of costs over sum of notionals, over the orders the engine marked trusted."""
    cost = notional = fees = Fraction(0)
    count = 0
    for row in rows_of(report["orders.csv"]):
        if row["arrival_mid_trusted"] != "true":
            continue
        ref = reference[row["order_id"]]
        assert ref["mid"] is not None and ref["filled"]
        sign = 1 if ref["side"] == "buy" else -1
        cost += sign * (ref["notional"] - ref["filled"] * ref["mid"])
        notional += ref["filled"] * ref["mid"]
        fees += ref["fees"]
        count += 1
    (totals,) = rows_of(report["summary_by_all.csv"])
    assert int(totals["orders_costed"]) == count
    assert close(totals["slippage_cost"], cost)
    assert close(totals["slippage_bps"], cost / notional * 10_000)
    assert close(totals["fee_bps"], fees / notional * 10_000)
    assert close(totals["total_bps"], (cost + fees) / notional * 10_000)
    all_filled = sum(ref["filled"] for ref in reference.values())
    all_ordered = sum(ref["qty"] for ref in reference.values())
    assert close(totals["fill_rate"], Fraction(all_filled, all_ordered))


def test_the_worked_example_matches_the_hand_calculation_in_the_docs(tmp_path):
    """docs/methodology.md walks through these numbers; the CLI must print the same ones.

    W-BUY: arrival quote 100.00 / 100.04, mid 100.02. Fills 400 @ 100.05 and 600 @ 100.10.
      shortfall = 400 x 0.03 + 600 x 0.08 = 12 + 48 = 60.00
      notional  = 1000 x 100.02 = 100,020 -> 60 / 100,020 x 10,000 = 5.9988 bps
      fees 3.00 -> all-in 63.00 = 6.2987 bps
    W-SELL: same market, fills 400 @ 100.01 and 600 @ 100.06.
      shortfall = -(400 x -0.01 + 600 x 0.04) = -(-4 + 24) = -20.00 (sold above the mid)
      -20 / 100,020 x 10,000 = -1.9996 bps; fees 3.00 -> all-in -17.00
    """
    out = tmp_path / "worked"
    assert main(["report", "--data", str(WORKED_DIR), "--out", str(out)]) == 0
    rows = {row["order_id"]: row for row in rows_of((out / "orders.csv").read_text())}

    buy = rows["W-BUY"]
    assert buy["arrival_mid"] == "100.020000"
    assert buy["avg_price"] == "100.080000"
    assert (buy["arrival_mid_cost"], buy["arrival_mid_bps"]) == ("60.0000", "5.9988")
    assert (buy["spread_cost"], buy["drift_cost"]) == ("20.0000", "40.0000")
    assert (buy["fees"], buy["total_cost"], buy["total_bps"]) == ("3.0000", "63.0000", "6.2987")
    assert (buy["decision_mid"], buy["decision_mid_cost"]) == ("100.000000", "80.0000")
    assert (buy["interval_vwap"], buy["interval_vwap_cost"]) == ("100.060000", "20.0000")
    assert buy["trusted"] == "true" and buy["flags"] == ""

    sell = rows["W-SELL"]
    assert sell["arrival_mid"] == "100.020000"
    assert sell["avg_price"] == "100.040000"
    assert (sell["arrival_mid_cost"], sell["arrival_mid_bps"]) == ("-20.0000", "-1.9996")
    assert (sell["spread_cost"], sell["drift_cost"]) == ("20.0000", "-40.0000")
    assert sell["total_cost"] == "-17.0000"
