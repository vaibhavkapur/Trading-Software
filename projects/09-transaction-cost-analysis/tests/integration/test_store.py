"""The SQLite store: idempotent writes, conflict rejection, content versions, restarts."""

from decimal import Decimal

from builders import FIXTURE_DIR, WORKED_DIR

from tca.importer import parse_csv, parse_file
from tca.service import TcaService
from tca.store import Store

QUOTES = (
    "symbol,venue,bid,ask,ts,recv_ts\n"
    "XYZ,V1,100.00,100.04,2026-03-02T15:00:00Z,\n"
    "XYZ,V1,100.01,100.05,2026-03-02T15:00:01Z,2026-03-02T15:00:01.002Z\n"
    "XYZ,V2,100.00,100.06,2026-03-02T15:00:01Z,\n"
)


def insert(store: Store, dataset: str, text: str):
    return store.insert(parse_csv(dataset, text), "test")


def test_rows_round_trip_with_exact_decimals():
    store = Store()
    result = insert(store, "quotes", QUOTES)
    assert (result.rows, result.accepted, result.duplicates, result.rejected) == (3, 3, 0, 0)
    _, dataset = store.snapshot()
    assert dataset.counts == {"orders": 0, "child_orders": 0, "fills": 0, "quotes": 3, "bars": 0}
    first = dataset.quotes[0]
    assert (first.bid, first.ask) == (Decimal("100.00"), Decimal("100.04"))
    assert first.recv_ts is None and dataset.quotes[1].recv_ts is not None


def test_redelivery_is_a_no_op():
    store = Store()
    insert(store, "quotes", QUOTES)
    revision, before = store.snapshot()
    again = insert(store, "quotes", QUOTES)
    assert (again.accepted, again.duplicates, again.rejected) == (0, 3, 0)
    after_revision, after = store.snapshot()
    assert after.version == before.version and after.quotes == before.quotes
    assert after_revision == revision  # nothing changed, so caches stay valid


def test_equal_decimals_written_differently_are_the_same_row():
    store = Store()
    insert(store, "quotes", QUOTES)
    restated = QUOTES.replace("100.00,100.04", "100,100.040")
    assert insert(store, "quotes", restated).duplicates == 3


def test_a_different_row_under_an_existing_key_is_rejected_not_overwritten():
    store = Store()
    insert(store, "quotes", QUOTES)
    _, before = store.snapshot()
    changed = QUOTES.replace("100.00,100.04", "99.00,100.04")
    result = insert(store, "quotes", changed)
    assert (result.accepted, result.duplicates) == (0, 2)
    (reject,) = result.rejects
    assert (reject.row, reject.code) == (1, "CONFLICT")
    assert "XYZ, V1" in reject.detail
    assert store.snapshot()[1].version == before.version


def test_duplicates_inside_one_batch_collapse():
    store = Store()
    line = "F1,O1,,V1,100,100.05,0.30,2026-03-02T15:00:05Z\n"
    header = "fill_id,order_id,child_id,venue,qty,price,fee,ts\n"
    result = insert(store, "fills", header + line + line + line.replace("100.05", "100.06"))
    assert (result.accepted, result.duplicates, result.rejected) == (1, 1, 1)
    assert result.rejects[0].code == "CONFLICT"
    assert store.snapshot()[1].fills[0].price == Decimal("100.05")


def test_parse_rejects_and_store_rejects_are_merged_in_row_order():
    store = Store()
    insert(store, "quotes", QUOTES)
    text = (
        "symbol,venue,bid,ask,ts,recv_ts\n"
        "XYZ,V1,99.00,100.04,2026-03-02T15:00:00Z,\n"  # conflicts with stored row
        "XYZ,V1,101.00,100.04,2026-03-02T15:00:09Z,\n"  # crossed: rejected at parse
        "XYZ,V1,100.02,100.06,2026-03-02T15:00:02Z,\n"  # fine
    )
    result = insert(store, "quotes", text)
    assert result.accepted == 1
    assert [(r.row, r.code) for r in result.rejects] == [(1, "CONFLICT"), (2, "CROSSED_QUOTE")]


def test_bar_interval_must_stay_consistent_per_symbol():
    store = Store()
    header = "symbol,ts,interval_s,vwap,volume\n"
    insert(store, "bars", header + "XYZ,2026-03-02T15:00:00Z,60,100.04,12000\n")
    result = insert(
        store,
        "bars",
        header
        + "XYZ,2026-03-02T15:05:00Z,300,100.04,12000\n"  # XYZ is a 60s series
        + "ABC,2026-03-02T15:05:00Z,300,50.00,900\n"  # a different symbol may differ
        + "ABC,2026-03-02T15:06:00Z,60,50.00,900\n",  # but not within the same batch
    )
    assert result.accepted == 1
    assert [(r.row, r.code) for r in result.rejects] == [
        (1, "BAR_INTERVAL_MISMATCH"),
        (3, "BAR_INTERVAL_MISMATCH"),
    ]


def test_version_depends_on_content_not_on_load_order_or_batching():
    files = {name: parse_file(name, FIXTURE_DIR / f"{name}.csv") for name in ("orders", "fills")}

    forward = Store()
    forward.insert(files["orders"])
    forward.insert(files["fills"])

    shuffled = Store()
    half = len(files["fills"].records) // 2
    late, early = files["fills"].records[half:], files["fills"].records[:half]
    for records in (late, early):
        batch = parse_csv("fills", "fill_id,order_id,child_id,venue,qty,price,fee,ts\n")
        batch.records = list(reversed(records))
        shuffled.insert(batch)
    shuffled.insert(files["orders"])

    a, b = forward.snapshot()[1], shuffled.snapshot()[1]
    assert a.version == b.version and a.versions == b.versions
    assert a.fills == b.fills


def test_version_changes_when_any_dataset_changes():
    store = Store()
    empty = store.snapshot()[1]
    insert(store, "quotes", QUOTES)
    loaded = store.snapshot()[1]
    assert loaded.version != empty.version
    assert loaded.versions["quotes"] != empty.versions["quotes"]
    assert loaded.versions["orders"] == empty.versions["orders"]
    assert loaded.version.startswith("sha256:") and len(loaded.version) == 71


def test_data_and_versions_survive_a_restart(tmp_path):
    path = str(tmp_path / "tca.sqlite3")
    first = TcaService(Store(path))
    first.import_directory(WORKED_DIR)
    before = first.analysis()
    first.store.close()

    second = TcaService(Store(path))
    after = second.analysis()
    assert after.dataset.version == before.dataset.version
    assert after.dataset.counts == before.dataset.counts
    assert [r.slippage["arrival_mid"].cost for r in after.results] == [
        r.slippage["arrival_mid"].cost for r in before.results
    ]
    # Re-running the same import after a restart changes nothing.
    assert all(r.accepted == 0 for r in second.import_directory(WORKED_DIR))
    assert second.analysis().dataset.version == before.dataset.version


def test_import_history_is_recorded():
    store = Store()
    insert(store, "quotes", QUOTES)
    insert(store, "quotes", QUOTES)
    newest, oldest = store.imports()
    assert (oldest["accepted"], oldest["duplicates"]) == (3, 0)
    assert (newest["accepted"], newest["duplicates"]) == (0, 3)
    assert newest["dataset"] == "quotes" and newest["source"] == "test"
