"""SQLite-backed store for the normalised input datasets.

Writes are idempotent on each dataset's natural key: delivering the same row again is a
no-op, and delivering a *different* row under an existing key is rejected rather than
overwriting history. Dataset versions are content hashes, so the same data loaded in any
order or batching yields the same version string.
"""

from __future__ import annotations

import hashlib
import sqlite3
import threading
from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime
from decimal import Decimal
from typing import Any

from tca.domain import DATASETS, Bar, ChildOrder, Fill, ParentOrder, Quote, Side
from tca.importer import ParsedBatch, Reject

_SCHEMA = """
CREATE TABLE IF NOT EXISTS orders (
    order_id TEXT PRIMARY KEY, symbol TEXT NOT NULL, side TEXT NOT NULL, qty INTEGER NOT NULL,
    decision_ts INTEGER NOT NULL, arrival_ts INTEGER NOT NULL,
    broker TEXT NOT NULL, strategy TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS child_orders (
    child_id TEXT PRIMARY KEY, order_id TEXT NOT NULL, venue TEXT NOT NULL,
    qty INTEGER NOT NULL, sent_ts INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS fills (
    fill_id TEXT PRIMARY KEY, order_id TEXT NOT NULL, child_id TEXT, venue TEXT NOT NULL,
    qty INTEGER NOT NULL, price TEXT NOT NULL, fee TEXT NOT NULL, ts INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS quotes (
    symbol TEXT NOT NULL, venue TEXT NOT NULL, ts INTEGER NOT NULL,
    bid TEXT NOT NULL, ask TEXT NOT NULL, recv_ts INTEGER,
    PRIMARY KEY (symbol, venue, ts)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS bars (
    symbol TEXT NOT NULL, ts INTEGER NOT NULL, interval_s INTEGER NOT NULL,
    vwap TEXT NOT NULL, volume INTEGER NOT NULL,
    PRIMARY KEY (symbol, ts)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS imports (
    id INTEGER PRIMARY KEY AUTOINCREMENT, dataset TEXT NOT NULL, source TEXT NOT NULL,
    rows INTEGER NOT NULL, accepted INTEGER NOT NULL, duplicates INTEGER NOT NULL,
    rejected INTEGER NOT NULL, imported_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
INSERT OR IGNORE INTO meta (key, value) VALUES ('revision', 0);
"""


def _dec(value: Decimal) -> str:
    """Canonical text so 100.10 and 100.1 compare and hash as the same price."""
    return "0" if value == 0 else format(value.normalize(), "f")


@dataclass(frozen=True)
class _Table:
    columns: tuple[str, ...]
    key: tuple[str, ...]
    to_row: Callable[[Any], tuple]
    from_row: Callable[[tuple], Any]


_TABLES: dict[str, _Table] = {
    "orders": _Table(
        (
            "order_id",
            "symbol",
            "side",
            "qty",
            "decision_ts",
            "arrival_ts",
            "broker",
            "strategy",
        ),
        ("order_id",),
        lambda o: (
            o.order_id,
            o.symbol,
            o.side.value,
            o.qty,
            o.decision_ts,
            o.arrival_ts,
            o.broker,
            o.strategy,
        ),
        lambda r: ParentOrder(r[0], r[1], Side(r[2]), r[3], r[4], r[5], r[6], r[7]),
    ),
    "child_orders": _Table(
        ("child_id", "order_id", "venue", "qty", "sent_ts"),
        ("child_id",),
        lambda c: (c.child_id, c.order_id, c.venue, c.qty, c.sent_ts),
        lambda r: ChildOrder(*r),
    ),
    "fills": _Table(
        ("fill_id", "order_id", "child_id", "venue", "qty", "price", "fee", "ts"),
        ("fill_id",),
        lambda f: (
            f.fill_id,
            f.order_id,
            f.child_id,
            f.venue,
            f.qty,
            _dec(f.price),
            _dec(f.fee),
            f.ts,
        ),
        lambda r: Fill(r[0], r[1], r[2], r[3], r[4], Decimal(r[5]), Decimal(r[6]), r[7]),
    ),
    "quotes": _Table(
        ("symbol", "venue", "ts", "bid", "ask", "recv_ts"),
        ("symbol", "venue", "ts"),
        lambda q: (q.symbol, q.venue, q.ts, _dec(q.bid), _dec(q.ask), q.recv_ts),
        lambda r: Quote(r[0], r[1], Decimal(r[3]), Decimal(r[4]), r[2], r[5]),
    ),
    "bars": _Table(
        ("symbol", "ts", "interval_s", "vwap", "volume"),
        ("symbol", "ts"),
        lambda b: (b.symbol, b.ts, b.interval_s, _dec(b.vwap), b.volume),
        lambda r: Bar(r[0], r[1], r[2], Decimal(r[3]), r[4]),
    ),
}
assert tuple(_TABLES) == DATASETS


@dataclass(frozen=True)
class ImportResult:
    dataset: str
    source: str
    rows: int
    accepted: int
    duplicates: int
    rejects: tuple[Reject, ...]

    @property
    def rejected(self) -> int:
        return len(self.rejects)


@dataclass(frozen=True)
class Dataset:
    """An immutable snapshot of everything in the store, plus its content versions."""

    orders: tuple[ParentOrder, ...]
    child_orders: tuple[ChildOrder, ...]
    fills: tuple[Fill, ...]
    quotes: tuple[Quote, ...]
    bars: tuple[Bar, ...]
    versions: dict[str, str]  # dataset name -> "sha256:<hex>"
    version: str  # hash over all of the above

    @property
    def counts(self) -> dict[str, int]:
        return {name: len(getattr(self, name)) for name in DATASETS}


class Store:
    def __init__(self, path: str = ":memory:"):
        self._conn = sqlite3.connect(path, check_same_thread=False, isolation_level=None)
        self._lock = threading.RLock()
        self._conn.executescript(_SCHEMA)

    def close(self) -> None:
        with self._lock:
            self._conn.close()

    def revision(self) -> int:
        """Bumped whenever an import changes stored data; used to invalidate caches."""
        with self._lock:
            return self._conn.execute("SELECT value FROM meta WHERE key='revision'").fetchone()[0]

    def insert(self, batch: ParsedBatch, source: str = "") -> ImportResult:
        table = _TABLES[batch.dataset]
        where = " AND ".join(f"{column} = ?" for column in table.key)
        key_index = [table.columns.index(column) for column in table.key]
        select = f"SELECT {', '.join(table.columns)} FROM {batch.dataset} WHERE {where}"
        insert = (
            f"INSERT INTO {batch.dataset} ({', '.join(table.columns)}) "
            f"VALUES ({', '.join('?' * len(table.columns))})"
        )
        rejects = list(batch.rejects)
        accepted = duplicates = 0

        with self._lock:
            self._conn.execute("BEGIN IMMEDIATE")
            try:
                bar_intervals = (
                    dict(self._conn.execute("SELECT symbol, MIN(interval_s) FROM bars GROUP BY 1"))
                    if batch.dataset == "bars"
                    else {}
                )
                for number, record in batch.records:
                    row = table.to_row(record)
                    existing = self._conn.execute(select, [row[i] for i in key_index]).fetchone()
                    if existing is not None:
                        if existing == row:
                            duplicates += 1
                        else:
                            key = ", ".join(str(row[i]) for i in key_index)
                            rejects.append(
                                Reject(
                                    number,
                                    "CONFLICT",
                                    f"{batch.dataset} key ({key}) already holds different values",
                                )
                            )
                        continue
                    if batch.dataset == "bars":
                        known = bar_intervals.setdefault(record.symbol, record.interval_s)
                        if known != record.interval_s:
                            rejects.append(
                                Reject(
                                    number,
                                    "BAR_INTERVAL_MISMATCH",
                                    f"{record.symbol} bars are {known}s; got {record.interval_s}s",
                                )
                            )
                            continue
                    self._conn.execute(insert, row)
                    accepted += 1
                if accepted:
                    self._conn.execute("UPDATE meta SET value = value + 1 WHERE key='revision'")
                self._conn.execute(
                    "INSERT INTO imports (dataset, source, rows, accepted, duplicates, rejected,"
                    " imported_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
                    (
                        batch.dataset,
                        source,
                        batch.rows,
                        accepted,
                        duplicates,
                        len(rejects),
                        datetime.now(UTC).isoformat(timespec="seconds"),
                    ),
                )
                self._conn.execute("COMMIT")
            except BaseException:
                self._conn.execute("ROLLBACK")
                raise

        rejects.sort(key=lambda reject: reject.row)
        return ImportResult(batch.dataset, source, batch.rows, accepted, duplicates, tuple(rejects))

    def snapshot(self) -> tuple[int, Dataset]:
        """Read every dataset at one consistent revision."""
        with self._lock:
            self._conn.execute("BEGIN")
            try:
                revision = self.revision()
                loaded: dict[str, tuple] = {}
                versions: dict[str, str] = {}
                for name, table in _TABLES.items():
                    rows = self._conn.execute(
                        f"SELECT {', '.join(table.columns)} FROM {name} "
                        f"ORDER BY {', '.join(table.key)}"
                    ).fetchall()
                    digest = hashlib.sha256()
                    for row in rows:
                        line = "\x1f".join("" if value is None else str(value) for value in row)
                        digest.update(line.encode() + b"\n")
                    versions[name] = f"sha256:{digest.hexdigest()}"
                    loaded[name] = tuple(table.from_row(row) for row in rows)
            finally:
                self._conn.execute("COMMIT")
        combined = hashlib.sha256(
            "\n".join(f"{name}={versions[name]}" for name in DATASETS).encode()
        ).hexdigest()
        return revision, Dataset(**loaded, versions=versions, version=f"sha256:{combined}")

    def imports(self, limit: int = 50) -> list[dict[str, Any]]:
        with self._lock:
            cursor = self._conn.execute(
                "SELECT id, dataset, source, rows, accepted, duplicates, rejected, imported_at"
                " FROM imports ORDER BY id DESC LIMIT ?",
                (limit,),
            )
            names = [column[0] for column in cursor.description]
            return [dict(zip(names, row, strict=True)) for row in cursor.fetchall()]
