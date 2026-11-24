"""Command line: import into a database, report from it, regenerate the fixture."""

import csv
import importlib.util
import json
from datetime import datetime
from decimal import Decimal

import pytest
from builders import FIXTURE_DIR, ROOT, WORKED_DIR

from tca.__main__ import main
from tca.service import TcaService
from tca.store import Store


def test_report_from_a_data_directory(tmp_path, capsys):
    out = tmp_path / "report"
    assert main(["report", "--data", str(WORKED_DIR), "--out", str(out)]) == 0
    printed = capsys.readouterr().out
    manifest = json.loads((out / "manifest.json").read_text())
    assert manifest["report_id"] in printed and manifest["dataset"]["version"] in printed
    with (out / "orders.csv").open() as handle:
        rows = {row["order_id"]: row for row in csv.DictReader(handle)}
    assert rows["W-BUY"]["arrival_mid_bps"] == "5.9988"
    assert rows["W-SELL"]["arrival_mid_bps"] == "-1.9996"
    assert {path.name for path in out.iterdir()} == set(manifest["files"]) | {"manifest.json"}


def test_import_then_report_from_a_database_matches_a_direct_report(tmp_path, capsys):
    db = str(tmp_path / "tca.sqlite3")
    assert main(["import", str(WORKED_DIR), "--db", db]) == 0
    assert main(["import", str(WORKED_DIR), "--db", db, "--strict"]) == 0  # duplicates are fine
    assert "duplicates=4" in capsys.readouterr().out

    from_db, direct = tmp_path / "from_db", tmp_path / "direct"
    assert main(["report", "--db", db, "--out", str(from_db)]) == 0
    assert main(["report", "--data", str(WORKED_DIR), "--out", str(direct)]) == 0
    for path in direct.iterdir():
        assert (from_db / path.name).read_bytes() == path.read_bytes(), path.name


def test_report_options_are_recorded(tmp_path):
    out = tmp_path / "report"
    args = ["report", "--data", str(WORKED_DIR), "--out", str(out)]
    assert main([*args, "--benchmark", "interval_vwap", "--include-untrusted"]) == 0
    options = json.loads((out / "manifest.json").read_text())["options"]
    assert options == {
        "benchmark": "interval_vwap",
        "include_untrusted": True,
        "bucket_minutes": 30,
    }


def test_strict_import_fails_when_rows_are_rejected(tmp_path, capsys):
    data = tmp_path / "data"
    data.mkdir()
    (data / "orders.csv").write_text(
        (WORKED_DIR / "orders.csv").read_text() + "BAD,XYZ,hold,1,x,y,B,S\n"
    )
    db = str(tmp_path / "tca.sqlite3")
    assert main(["import", str(data), "--db", db, "--strict"]) == 1
    assert "row 3: BAD_SIDE" in capsys.readouterr().out
    assert main(["import", str(data), "--db", db]) == 0  # without --strict it is a warning


def test_errors_exit_non_zero_with_a_message(tmp_path, capsys):
    empty = tmp_path / "empty"
    empty.mkdir()
    assert main(["report", "--data", str(empty), "--out", str(tmp_path / "r")]) == 2
    assert "no dataset files" in capsys.readouterr().err
    assert main(["report", "--out", str(tmp_path / "r")]) == 1
    assert "no orders to report on" in capsys.readouterr().err

    bad = tmp_path / "bad"
    bad.mkdir()
    (bad / "orders.csv").write_text("order_id,symbol\nO1,XYZ\n")
    assert main(["report", "--data", str(bad), "--out", str(tmp_path / "r")]) == 2
    assert "missing required columns" in capsys.readouterr().err


def test_simulate_command_regenerates_the_committed_fixture(tmp_path, capsys):
    assert main(["simulate", "--out", str(tmp_path)]) == 0
    for name in ("orders", "child_orders", "fills", "quotes", "bars"):
        assert (tmp_path / f"{name}.csv").read_bytes() == (FIXTURE_DIR / f"{name}.csv").read_bytes()


def test_parquet_files_load_to_the_same_dataset_as_csv(tmp_path):
    pa = pytest.importorskip("pyarrow")
    pq = pytest.importorskip("pyarrow.parquet")

    def timestamp(text):
        return datetime.fromisoformat(text) if text else None

    converters = {
        "qty": int,
        "volume": int,
        "interval_s": int,
        "price": Decimal,
        "fee": Decimal,
        "vwap": Decimal,
        "bid": float,  # doubles, as many market-data dumps store them
        "ask": float,
    }
    for name in ("orders", "fills", "quotes", "bars"):
        with (WORKED_DIR / f"{name}.csv").open() as handle:
            rows = list(csv.DictReader(handle))
        columns = {}
        for column in rows[0]:
            convert = timestamp if column.endswith("ts") else converters.get(column, str)
            columns[column] = [convert(row[column]) for row in rows]
        table = pa.table(columns)
        if name == "quotes":
            assert table.schema.field("ts").type.tz == "UTC"
            assert pa.types.is_floating(table.schema.field("bid").type)
        pq.write_table(table, tmp_path / f"{name}.parquet")

    from_parquet, from_csv = TcaService(Store()), TcaService(Store())
    for result in from_parquet.import_directory(tmp_path):
        assert result.rejected == 0 and result.source.endswith(".parquet")
    from_csv.import_directory(WORKED_DIR)
    assert from_parquet.analysis().dataset.version == from_csv.analysis().dataset.version


def test_naive_parquet_timestamps_are_rejected(tmp_path):
    pa = pytest.importorskip("pyarrow")
    pq = pytest.importorskip("pyarrow.parquet")
    table = pa.table(
        {
            "symbol": ["XYZ"],
            "venue": ["V1"],
            "bid": [Decimal("100.00")],
            "ask": [Decimal("100.04")],
            "ts": [datetime(2026, 3, 2, 15, 0, 0)],  # no timezone
        }
    )
    pq.write_table(table, tmp_path / "quotes.parquet")
    (result,) = TcaService(Store()).import_directory(tmp_path)
    assert result.accepted == 0 and result.rejects[0].code == "BAD_TIMESTAMP"


def test_demo_walks_through_the_fixture_and_writes_a_report(tmp_path, capsys):
    out = tmp_path / "demo"
    assert main(["demo", "--data", str(FIXTURE_DIR), "--out", str(out)]) == 0
    printed = capsys.readouterr().out
    assert "simulated data only" in printed
    assert "engine reports" in printed and "MISMATCH" not in printed
    for flag in ("ARRIVAL_QUOTE_MISSING", "ARRIVAL_QUOTE_STALE", "ARRIVAL_CLOCK_SKEW"):
        assert flag in printed
    assert "never used to backfill" in printed
    assert "dataset version unchanged" in printed
    assert json.loads((out / "manifest.json").read_text())["dataset"]["version"] in printed


def load_reconcile():
    path = ROOT / "scripts" / "reconcile.py"
    spec = importlib.util.spec_from_file_location("reconcile", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_the_standalone_reconciler_agrees_with_a_report_and_catches_tampering(tmp_path, capsys):
    reconcile = load_reconcile()
    report = tmp_path / "report"
    assert main(["report", "--data", str(FIXTURE_DIR), "--out", str(report)]) == 0
    args = ["--data", str(FIXTURE_DIR), "--report", str(report)]
    assert reconcile.main(args) == 0
    assert "52 orders recomputed" in capsys.readouterr().out

    # Flip the sign of one reported cost: both the hash and the recomputation object.
    orders = report / "orders.csv"
    text = orders.read_text()
    assert ",46.0000,5.4128," in text
    orders.write_text(text.replace(",46.0000,5.4128,", ",-46.0000,-5.4128,"))
    assert reconcile.main(args) == 1
    printed = capsys.readouterr().out
    assert "orders.csv: content does not match the manifest hash" in printed
    assert "P0004: arrival_mid_cost -46.0000" in printed
