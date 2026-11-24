import csv
import hashlib
import io
import json
from decimal import Decimal as D

from builders import bar, child, fill, order, quote, run

from tca.aggregate import GROUP_BYS, summarize
from tca.benchmark import BENCHMARKS, Methodology
from tca.render import (
    BENCHMARK_COLUMNS,
    FILL_COLUMNS,
    ORDER_COLUMNS,
    SUMMARY_COLUMNS,
    benchmark_row,
    fill_row,
    ms,
    num,
    order_row,
    order_view,
    overlay_view,
    summary_row,
)
from tca.report import build_bundle, methodology_view, orders_csv, to_csv, write_bundle

QUOTES = (
    quote("100.00", "100.04", -1),
    quote("100.01", "100.05", 4),
    quote("100.06", "100.10", 79),
)


def analysis(**kwargs):
    return run(
        [order("O1", decision=-0.5), order("O2", side="sell", arrival=200)],
        [
            fill("F1", 400, "100.05", 5, fee="1.20", child_id="C1"),
            fill("F2", 600, "100.10", 80, fee="1.80"),
        ],
        QUOTES,
        (bar(0, "100.04", 12_000), bar(1, "100.09", 8_000)),
        child_orders=[child("C1", "V1", 400, 4)],
        **kwargs,
    )


def parse(text):
    return list(csv.DictReader(io.StringIO(text)))


# -- number formatting --------------------------------------------------------------------


def test_num_rounds_half_even_to_fixed_places():
    assert num(D("5.99880024"), 4) == "5.9988"
    assert num(D("1.00005"), 4) == "1.0000"  # half-even: ties go to the even digit
    assert num(D("1.00015"), 4) == "1.0002"
    assert num(D("100.08"), 6) == "100.080000"
    assert num(7, 2) == "7.00"
    assert num(None, 4) is None


def test_num_never_prints_negative_zero_or_exponents():
    assert num(D("-0.00001"), 4) == "0.0000"
    assert num(D("-0.00006"), 4) == "-0.0001"
    assert num(D("1E+3"), 2) == "1000.00"
    assert num(D("1E-7"), 4) == "0.0000"


def test_ms_converts_microseconds():
    assert ms(5_000_000) == "5000.000"
    assert ms(1_234) == "1.234"
    assert ms(None) is None


# -- row shapes ---------------------------------------------------------------------------


def test_row_keys_match_the_declared_csv_columns():
    a = analysis()
    result = a.by_id["O1"]
    assert tuple(order_row(result, a)) == ORDER_COLUMNS
    assert tuple(fill_row(result.fills[0], result)) == FILL_COLUMNS
    assert tuple(benchmark_row("O1", result.benchmarks["arrival_mid"], "m")) == BENCHMARK_COLUMNS
    (group,) = summarize(a.results)
    assert tuple(summary_row(group, "all", "arrival_mid")) == SUMMARY_COLUMNS


def test_order_row_values_are_strings_with_versions_attached():
    a = analysis()
    row = order_row(a.by_id["O1"], a)
    assert row["arrival_mid"] == "100.020000"
    assert row["arrival_mid_bps"] == "5.9988"
    assert row["arrival_mid_cost"] == "60.0000"
    assert row["spread_bps"] == "1.9996" and row["drift_bps"] == "3.9992"
    assert row["fee_bps"] == "0.2999" and row["total_bps"] == "6.2987"
    assert row["fill_rate"] == "1.000000"
    assert row["time_to_first_fill_ms"] == "5000.000"
    assert row["arrival_ts"] == "2026-03-02T15:00:00.000000Z"
    assert row["trusted"] is True and row["flags"] == []
    assert row["methodology_version"] == a.methodology.id
    assert row["dataset_version"] == a.dataset.version


def test_unfilled_order_row_has_blanks_not_zeros():
    a = analysis()
    row = order_row(a.by_id["O2"], a)
    assert row["filled_qty"] == 0 and row["fill_rate"] == "0.000000"
    for column in ("avg_price", "arrival_mid_bps", "total_bps", "time_to_first_fill_ms"):
        assert row[column] is None
    assert row["flags"][0] == "NO_FILLS"


def test_order_view_headline_follows_the_requested_benchmark():
    a = analysis()
    view = order_view(a.by_id["O1"], a, "interval_vwap")
    assert view["headline"] == {
        "benchmark": "interval_vwap",
        "value": "100.060000",
        "status": "ok",
        "cost": "20.0000",
        "bps": "1.9988",
        "trusted": True,
    }
    assert [b["name"] for b in view["benchmarks"]] == list(BENCHMARKS)
    arrival = view["benchmarks"][0]
    assert arrival["details"]["quote_ts"] == "2026-03-02T14:59:59.000000Z"
    assert arrival["details"]["quote_age_ms"] == "1000.000"
    assert arrival["details"]["bid"] == "100.000000"
    assert arrival["methodology_version"] == a.methodology.id
    assert len(view["fills"]) == 2 and view["child_orders"][0]["child_id"] == "C1"


def test_overlay_brackets_the_order_and_carries_benchmark_levels():
    a = analysis()
    overlay = overlay_view(a.by_id["O1"], a)
    assert overlay["start_ts"] < overlay["decision_ts"] < overlay["arrival_ts"]
    assert overlay["last_fill_ts"] < overlay["end_ts"]
    assert overlay["levels"] == {
        "arrival_mid": "100.020000",
        "decision_mid": "100.020000",
        "interval_vwap": "100.060000",
        "avg_price": "100.080000",
    }
    assert [q["bid"] for q in overlay["quotes"]] == ["100.000000", "100.010000", "100.060000"]
    assert len(overlay["bars"]) == 2 and len(overlay["fills"]) == 2
    assert overlay["vwap_window"]["end_ts"] == "2026-03-02T15:02:00.000000Z"

    unfilled = overlay_view(a.by_id["O2"], a)
    assert unfilled["fills"] == [] and unfilled["levels"]["avg_price"] is None
    assert unfilled["vwap_window"] == {"start_ts": None, "end_ts": None}


# -- CSV ----------------------------------------------------------------------------------


def test_csv_cells():
    text = to_csv(
        [{"a": None, "b": True, "c": ["X", "Y"], "d": {"k": 1}, "e": "has,comma"}],
        ["a", "b", "c", "d", "e"],
    )
    assert text == 'a,b,c,d,e\n,true,X;Y,"{""k"":1}","has,comma"\n'


def test_empty_export_still_has_the_full_header():
    a = analysis()
    assert orders_csv([], a) == ",".join(ORDER_COLUMNS) + "\n"


# -- bundle -------------------------------------------------------------------------------


def test_bundle_is_deterministic_and_self_describing():
    first, second = build_bundle(analysis()), build_bundle(analysis())
    assert first == second
    expected_files = {"orders.csv", "fills.csv", "benchmarks.csv", "manifest.json"} | {
        f"summary_by_{group_by}.csv" for group_by in GROUP_BYS
    }
    assert set(first) == expected_files

    manifest = json.loads(first["manifest.json"])
    assert manifest["methodology"]["id"] == Methodology().id
    assert manifest["methodology"]["parameters"]["max_quote_age_ms"] == 5000
    assert manifest["dataset"]["version"] == "sha256:test"
    assert manifest["dataset"]["rows"]["orders"] == 2
    assert manifest["quality"]["orders_filled"] == 1
    assert manifest["quality"]["orders_trusted"]["arrival_mid"] == 1
    assert manifest["quality"]["flag_counts"]["NO_FILLS"] == 1
    for name, entry in manifest["files"].items():
        digest = hashlib.sha256(first[name].encode()).hexdigest()
        assert entry["sha256"] == f"sha256:{digest}"
        assert entry["rows"] == len(parse(first[name]))


def test_bundle_identity_tracks_methodology_and_options():
    base = json.loads(build_bundle(analysis())["manifest.json"])
    strict = json.loads(
        build_bundle(analysis(methodology=Methodology(max_quote_age_ms=500)))["manifest.json"]
    )
    by_vwap = json.loads(build_bundle(analysis(), benchmark="interval_vwap")["manifest.json"])
    assert len({base["report_id"], strict["report_id"], by_vwap["report_id"]}) == 3
    assert strict["methodology"]["id"] != base["methodology"]["id"]
    assert by_vwap["options"]["benchmark"] == "interval_vwap"


def test_every_order_and_benchmark_row_names_its_methodology(tmp_path):
    manifest = write_bundle(analysis(), tmp_path / "report")
    orders = parse((tmp_path / "report" / "orders.csv").read_text())
    benchmarks = parse((tmp_path / "report" / "benchmarks.csv").read_text())
    assert len(orders) == 2 and len(benchmarks) == 6
    assert {row["methodology_version"] for row in orders + benchmarks} == {
        manifest["methodology"]["id"]
    }
    assert {row["dataset_version"] for row in orders} == {manifest["dataset"]["version"]}
    assert json.loads((tmp_path / "report" / "manifest.json").read_text()) == manifest


def test_methodology_view_documents_assumptions():
    view = methodology_view(Methodology())
    assert "positive when the execution was worse" in view["sign_convention"]
    assert set(view["benchmarks"]) == set(BENCHMARKS)
    assert view["rounding"]["mode"] == "half-even"
