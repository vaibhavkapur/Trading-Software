"""HTTP contract: imports in, results and exports out."""

import csv
import io
import json

import pytest
from builders import FIXTURE_DIR, WORKED_DIR
from fastapi.testclient import TestClient

from tca import api
from tca.api import create_app
from tca.benchmark import Methodology

DATASET_PATHS = {
    "orders": "orders",
    "child_orders": "child-orders",
    "fills": "fills",
    "quotes": "quotes",
    "bars": "bars",
}


def post_csv(client, dataset, text, **params):
    return client.post(
        f"/v1/imports/{dataset}", content=text, headers={"content-type": "text/csv"}, params=params
    )


def load_directory(client, directory):
    for name, path in DATASET_PATHS.items():
        file = directory / f"{name}.csv"
        if file.exists():
            response = post_csv(client, path, file.read_text(), source=file.name)
            assert response.status_code == 200, response.text
            assert response.json()["rejected"] == 0


@pytest.fixture
def client():
    return TestClient(create_app())


@pytest.fixture
def worked(client):
    load_directory(client, WORKED_DIR)
    return client


@pytest.fixture(scope="module")
def simulated():
    client = TestClient(create_app())
    load_directory(client, FIXTURE_DIR)
    return client


# -- imports ------------------------------------------------------------------------------


def test_import_reports_counts_and_is_idempotent(client):
    text = (WORKED_DIR / "fills.csv").read_text()
    first = post_csv(client, "fills", text, source="fills.csv").json()
    assert first == {
        "dataset": "fills",
        "source": "fills.csv",
        "rows": 4,
        "accepted": 4,
        "duplicates": 0,
        "rejected": 0,
        "rejects": [],
        "rejects_truncated": False,
    }
    again = post_csv(client, "fills", text).json()
    assert (again["accepted"], again["duplicates"]) == (0, 4)


def test_import_accepts_json_rows(client):
    rows = [
        {
            "symbol": "XYZ",
            "venue": "V1",
            "bid": 100.00,
            "ask": 100.04,
            "ts": "2026-03-02T15:00:00Z",
        },
        {
            "symbol": "XYZ",
            "venue": "V1",
            "bid": 100.05,
            "ask": 100.04,
            "ts": "2026-03-02T15:00:01Z",
        },
    ]
    response = client.post("/v1/imports/quotes", json={"rows": rows})
    body = response.json()
    assert response.status_code == 200
    assert (body["accepted"], body["rejected"]) == (1, 1)
    assert body["rejects"][0]["row"] == 2 and body["rejects"][0]["code"] == "CROSSED_QUOTE"


def test_partial_rejection_still_returns_200_with_reasons(client):
    text = (WORKED_DIR / "orders.csv").read_text() + "BAD,XYZ,buy,-5,x,y,B,S\n"
    body = post_csv(client, "orders", text).json()
    assert (body["rows"], body["accepted"], body["rejected"]) == (3, 2, 1)
    assert body["rejects"][0]["row"] == 3


@pytest.mark.parametrize(
    ("body", "content_type", "fragment"),
    [
        ("order_id,symbol\nO1,XYZ\n", "text/csv", "missing required columns"),
        ("", "text/csv", "empty payload"),
        ("{oops", "application/json", "not valid JSON"),
        (b"\xff\xfe\x00bad", "text/csv", "codec"),
    ],
)
def test_unreadable_payloads_are_400(client, body, content_type, fragment):
    response = client.post(
        "/v1/imports/orders", content=body, headers={"content-type": content_type}
    )
    assert response.status_code == 400
    assert fragment in response.json()["detail"]


def test_unknown_dataset_is_422(client):
    assert post_csv(client, "trades", "a,b\n").status_code == 422


def test_oversized_import_is_413(client, monkeypatch):
    monkeypatch.setattr(api, "MAX_IMPORT_BYTES", 64)
    response = post_csv(client, "fills", (WORKED_DIR / "fills.csv").read_text())
    assert response.status_code == 413
    assert client.get("/v1/datasets").json()["rows"]["fills"] == 0


# -- order results ------------------------------------------------------------------------


def test_order_detail_matches_the_worked_example(worked):
    body = worked.get("/v1/tca/orders/W-BUY").json()
    summary = body["summary"]
    assert summary["arrival_mid"] == "100.020000"
    assert summary["arrival_mid_bps"] == "5.9988"
    assert summary["spread_cost"] == "20.0000" and summary["drift_cost"] == "40.0000"
    assert body["headline"]["benchmark"] == "arrival_mid" and body["headline"]["bps"] == "5.9988"
    assert body["trusted"] is True and body["flags"] == [] and body["notes"] == []
    assert len(body["fills"]) == 2 and len(body["benchmarks"]) == 3
    assert body["methodology_version"] == Methodology().id
    assert body["dataset_version"].startswith("sha256:")


def test_benchmark_parameter_selects_the_headline(worked):
    vwap = worked.get("/v1/tca/orders/W-BUY", params={"benchmark": "interval_vwap"}).json()
    assert vwap["headline"] == {
        "benchmark": "interval_vwap",
        "value": "100.060000",
        "status": "ok",
        "cost": "20.0000",
        "bps": "1.9988",
        "trusted": True,
    }
    decision = worked.get("/v1/tca/orders/W-SELL", params={"benchmark": "decision_mid"}).json()
    assert decision["headline"]["bps"] == "-4.0000"


def test_unknown_order_and_benchmark(worked):
    assert worked.get("/v1/tca/orders/NOPE").status_code == 404
    assert worked.get("/v1/tca/orders/NOPE/overlay").status_code == 404
    assert worked.get("/v1/tca/orders/W-BUY", params={"benchmark": "close"}).status_code == 422


def test_overlay(worked):
    overlay = worked.get("/v1/tca/orders/W-BUY/overlay").json()
    assert overlay["symbol"] == "XYZ" and len(overlay["fills"]) == 2
    assert overlay["levels"]["interval_vwap"] == "100.060000"
    assert len(overlay["quotes"]) >= 5 and len(overlay["bars"]) == 2


def test_order_list_and_filters(simulated):
    everything = simulated.get("/v1/tca/orders").json()
    assert everything["count"] == 61 == len(everything["orders"])
    arrivals = [row["arrival_ts"] for row in everything["orders"]]
    assert arrivals == sorted(arrivals)

    def count(**params):
        return simulated.get("/v1/tca/orders", params=params).json()["count"]

    assert count(symbol="ACME") + count(symbol="GLOBX") + count(symbol="NOVA") == 61
    assert count(side="buy") + count(side="sell") == 61
    assert count(broker="NORTH", strategy="TWAP") <= count(strategy="TWAP")
    assert count(symbol="") == 61  # blank filters from the dashboard mean "any"
    assert count(symbol="NOPE") == 0
    early = count(end="2026-03-02T15:00:00Z")
    late = count(start="2026-03-02T15:00:00Z")
    assert early and late and early + late == 61
    assert count(start="2026-03-02T10:00:00-05:00") == late  # offsets are honoured
    assert 0 < count(venue="CHARLIE") < 61


def test_bad_filter_values_are_422(simulated):
    assert simulated.get("/v1/tca/orders", params={"start": "yesterday"}).status_code == 422
    assert (
        simulated.get("/v1/tca/orders", params={"start": "2026-03-02T15:00:00"}).status_code == 422
    )
    assert simulated.get("/v1/tca/orders", params={"side": "hold"}).status_code == 422


# -- summaries ----------------------------------------------------------------------------


def test_summary_by_venue(simulated):
    body = simulated.get("/v1/tca/summary", params={"group_by": "venue"}).json()
    assert [row["key"] for row in body["rows"]] == ["ALPHA", "BRAVO", "CHARLIE"]
    assert body["benchmark"] == "arrival_mid" and body["group_by"] == "venue"
    assert any("not normalised for order difficulty" in note for note in body["notes"])
    assert body["totals"]["orders"] == 61
    for row in body["rows"]:
        assert row["order_qty"] is None and row["routed_qty"] > 0
        assert 0 < float(row["fill_rate"]) <= 1
    filled = sum(row["filled_qty"] for row in body["rows"])
    assert filled == body["totals"]["filled_qty"]
    assert body["methodology_version"] == Methodology().id


@pytest.mark.parametrize("group_by", ["all", "broker", "symbol", "strategy", "side", "time_bucket"])
def test_whole_order_groupings_partition_the_orders(simulated, group_by):
    body = simulated.get("/v1/tca/summary", params={"group_by": group_by}).json()
    assert sum(row["orders"] for row in body["rows"]) == 61
    assert sum(row["orders_costed"] for row in body["rows"]) == body["totals"]["orders_costed"]
    cost = sum(float(row["slippage_cost"] or 0) for row in body["rows"])
    assert cost == pytest.approx(float(body["totals"]["slippage_cost"]), abs=1e-3)


def test_summary_options(simulated):
    def totals(**params):
        return simulated.get("/v1/tca/summary", params=params).json()["totals"]

    trusted, everything = totals(), totals(include_untrusted=True)
    assert trusted["orders_costed"] == trusted["orders_trusted"] < everything["orders_costed"]
    assert totals(benchmark="interval_vwap")["spread_bps"] is None
    assert totals(symbol="NOVA")["orders"] < trusted["orders"]

    buckets = simulated.get(
        "/v1/tca/summary", params={"group_by": "time_bucket", "bucket_minutes": 15}
    ).json()
    assert buckets["bucket_minutes"] == 15
    assert [row["key"][11:16] for row in buckets["rows"]] == [
        "14:30",
        "14:45",
        "15:00",
        "15:15",
        "15:30",
    ]
    assert simulated.get("/v1/tca/summary", params={"group_by": "desk"}).status_code == 422
    assert simulated.get("/v1/tca/summary", params={"bucket_minutes": 0}).status_code == 422


def test_empty_store_is_served_cleanly(client):
    assert client.get("/v1/tca/orders").json()["count"] == 0
    summary = client.get("/v1/tca/summary").json()
    assert summary["rows"] == [] and summary["totals"] is None
    datasets = client.get("/v1/datasets").json()
    assert datasets["rows"]["orders"] == 0 and datasets["arrival_range"]["start"] is None


# -- exports ------------------------------------------------------------------------------


def rows_of(response):
    assert response.status_code == 200
    assert response.headers["content-type"].startswith("text/csv")
    return list(csv.DictReader(io.StringIO(response.text)))


def test_order_export_carries_methodology_and_dataset_versions(simulated):
    response = simulated.get("/v1/tca/export.csv")
    rows = rows_of(response)
    assert 'filename="tca_orders.csv"' in response.headers["content-disposition"]
    assert len(rows) == 61
    datasets = simulated.get("/v1/datasets").json()
    assert {row["methodology_version"] for row in rows} == {Methodology().id}
    assert {row["dataset_version"] for row in rows} == {datasets["version"]}
    listed = simulated.get("/v1/tca/orders").json()["orders"]
    assert [row["arrival_mid_bps"] for row in rows] == [o["arrival_mid_bps"] or "" for o in listed]


def test_other_export_views_and_filters(simulated):
    fills = rows_of(simulated.get("/v1/tca/export.csv", params={"view": "fills"}))
    assert len(fills) == 310
    benchmarks = rows_of(simulated.get("/v1/tca/export.csv", params={"view": "benchmarks"}))
    assert len(benchmarks) == 61 * 3
    assert json.loads(benchmarks[0]["details"])["as_of_ts"].endswith("Z")
    nova = rows_of(simulated.get("/v1/tca/export.csv", params={"symbol": "NOVA"}))
    assert nova and {row["symbol"] for row in nova} == {"NOVA"}
    summary = rows_of(simulated.get("/v1/tca/summary.csv", params={"group_by": "strategy"}))
    assert [row["key"] for row in summary] == ["LIQUIDITY_SEEK", "PASSIVE", "TWAP", "VWAP"]


# -- metadata, dashboard, operations ------------------------------------------------------


def test_datasets_and_methodology(simulated):
    datasets = simulated.get("/v1/datasets").json()
    assert datasets["rows"] == {
        "orders": 61,
        "child_orders": 405,
        "fills": 310,
        "quotes": 14451,
        "bars": 266,
    }
    assert set(datasets["versions"]) == set(datasets["rows"])
    assert datasets["dimensions"]["venue"] == ["ALPHA", "BRAVO", "CHARLIE"]
    assert datasets["dimensions"]["symbol"] == ["ACME", "GLOBX", "NOVA"]
    assert datasets["orphan_fills"] == 0
    assert len(datasets["imports"]) == 5

    methodology = simulated.get("/v1/methodology").json()
    assert methodology["id"] == Methodology().id == datasets["methodology"]["id"]
    assert methodology["parameters"]["quote_clock"] == "event"


def test_results_refresh_when_more_data_arrives(client):
    post_csv(client, "orders", (WORKED_DIR / "orders.csv").read_text())
    post_csv(client, "fills", (WORKED_DIR / "fills.csv").read_text())
    before = client.get("/v1/tca/orders/W-BUY").json()
    assert "ARRIVAL_QUOTE_MISSING" in before["flags"] and before["headline"]["bps"] is None

    post_csv(client, "quotes", (WORKED_DIR / "quotes.csv").read_text())
    after = client.get("/v1/tca/orders/W-BUY").json()
    assert after["headline"]["bps"] == "5.9988"
    assert after["dataset_version"] != before["dataset_version"]


def test_dashboard_health_and_metrics(worked):
    page = worked.get("/")
    assert page.status_code == 200 and "Transaction Cost Analysis" in page.text
    assert worked.get("/healthz").json()["status"] == "ok"
    worked.get("/v1/tca/summary")

    metrics = worked.get("/metrics").text
    assert 'tca_import_rows_total{dataset="fills",outcome="accepted"} 4.0' in metrics
    assert 'tca_orders{state="filled"} 2.0' in metrics
    assert 'tca_trusted_benchmark_share{benchmark="arrival_mid"} 1.0' in metrics
    assert "tca_analysis_seconds_count 1.0" in metrics
    # The blueprint's "metrics to expose", for the two worked orders together:
    # shortfall (60 - 20) / 200,040 = 1.9996 bps, fees 6 / 200,040, spread 40 / 200,040.
    assert 'tca_shortfall_bps{benchmark="arrival_mid"} 1.9996' in metrics
    assert 'tca_cost_bps{component="fees",strategy="WORKED"} 0.2999' in metrics
    assert 'tca_cost_bps{component="spread",strategy="WORKED"} 1.9996' in metrics
    assert "tca_fill_rate 1.0" in metrics
    assert 'tca_time_to_fill_seconds{fill="first"} 5.0' in metrics
    assert 'tca_time_to_fill_seconds{fill="last"} 80.0' in metrics


def test_undefined_metrics_are_nan_not_zero(client):
    post_csv(client, "orders", (WORKED_DIR / "orders.csv").read_text())
    post_csv(client, "fills", (WORKED_DIR / "fills.csv").read_text())
    client.get("/v1/tca/orders")  # no quotes loaded, so no benchmark exists
    metrics = client.get("/metrics").text
    assert 'tca_shortfall_bps{benchmark="arrival_mid"} NaN' in metrics
    assert "tca_fill_rate 1.0" in metrics


def test_app_instances_do_not_share_state():
    first, second = TestClient(create_app()), TestClient(create_app())
    load_directory(first, WORKED_DIR)
    assert first.get("/v1/tca/orders").json()["count"] == 2
    assert second.get("/v1/tca/orders").json()["count"] == 0
