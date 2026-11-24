"""SQLite state store.

One connection guarded by a re-entrant lock: correct for a single-process deployment,
and uniqueness constraints (not the lock) are what enforce idempotency.
"""

from __future__ import annotations

import json
import sqlite3
import threading
from collections.abc import Iterator
from contextlib import contextmanager
from pathlib import Path
from typing import Any

from pydantic import BaseModel

SCHEMA = """
CREATE TABLE IF NOT EXISTS intents (
    id TEXT PRIMARY KEY,
    correlation_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    text TEXT NOT NULL,
    mode TEXT NOT NULL,
    symbol TEXT NOT NULL,
    side TEXT NOT NULL,
    notional TEXT,
    quantity INTEGER,
    order_type TEXT NOT NULL,
    limit_price TEXT,
    rationale TEXT NOT NULL,
    planner TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS proposals (
    id TEXT PRIMARY KEY,
    intent_id TEXT NOT NULL REFERENCES intents(id),
    correlation_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    mode TEXT NOT NULL,
    symbol TEXT NOT NULL,
    side TEXT NOT NULL,
    order_type TEXT NOT NULL,
    quantity INTEGER NOT NULL,
    limit_price TEXT,
    time_in_force TEXT NOT NULL,
    reference_price TEXT NOT NULL,
    quote_asof TEXT NOT NULL,
    quote_received_at TEXT NOT NULL,
    status TEXT NOT NULL,
    proposal_hash TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS proposals_user ON proposals(user_id, symbol, created_at);

CREATE TABLE IF NOT EXISTS risk_decisions (
    id TEXT PRIMARY KEY,
    proposal_id TEXT NOT NULL REFERENCES proposals(id),
    policy_version TEXT NOT NULL,
    stage TEXT NOT NULL,
    passed INTEGER NOT NULL,
    violations TEXT NOT NULL,
    checks TEXT NOT NULL,
    evaluated_at TEXT NOT NULL
);

-- One decision per proposal: a proposal can never be approved twice.
CREATE TABLE IF NOT EXISTS approvals (
    id TEXT PRIMARY KEY,
    proposal_id TEXT NOT NULL UNIQUE REFERENCES proposals(id),
    proposal_hash TEXT NOT NULL,
    approver_id TEXT NOT NULL,
    decision TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    signature TEXT NOT NULL
);

-- One order per proposal and per approval: duplicates collapse onto one row.
CREATE TABLE IF NOT EXISTS orders (
    client_order_id TEXT PRIMARY KEY,
    proposal_id TEXT NOT NULL UNIQUE REFERENCES proposals(id),
    approval_id TEXT NOT NULL UNIQUE REFERENCES approvals(id),
    correlation_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    symbol TEXT NOT NULL,
    side TEXT NOT NULL,
    order_type TEXT NOT NULL,
    quantity INTEGER NOT NULL,
    limit_price TEXT,
    state TEXT NOT NULL,
    broker_order_id TEXT,
    filled_qty INTEGER NOT NULL DEFAULT 0,
    avg_fill_price TEXT,
    submit_attempts INTEGER NOT NULL DEFAULT 0,
    reject_reason TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS fills (
    fill_id TEXT PRIMARY KEY,
    client_order_id TEXT NOT NULL REFERENCES orders(client_order_id),
    quantity INTEGER NOT NULL,
    price TEXT NOT NULL,
    venue_time TEXT NOT NULL,
    received_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_events (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    ts TEXT NOT NULL,
    correlation_id TEXT NOT NULL,
    actor TEXT NOT NULL,
    event_type TEXT NOT NULL,
    payload TEXT NOT NULL,
    prev_hash TEXT NOT NULL,
    hash TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_correlation ON audit_events(correlation_id, seq);

CREATE TRIGGER IF NOT EXISTS audit_no_update BEFORE UPDATE ON audit_events
BEGIN SELECT RAISE(ABORT, 'audit_events is append-only'); END;
CREATE TRIGGER IF NOT EXISTS audit_no_delete BEFORE DELETE ON audit_events
BEGIN SELECT RAISE(ABORT, 'audit_events is append-only'); END;

-- Paper broker's own books. Kept in separate tables: the OMS never reads them directly.
CREATE TABLE IF NOT EXISTS broker_accounts (
    user_id TEXT PRIMARY KEY,
    cash TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS broker_positions (
    user_id TEXT NOT NULL,
    symbol TEXT NOT NULL,
    quantity INTEGER NOT NULL,
    avg_cost TEXT NOT NULL,
    PRIMARY KEY (user_id, symbol)
);
CREATE TABLE IF NOT EXISTS broker_orders (
    broker_order_id TEXT PRIMARY KEY,
    client_order_id TEXT NOT NULL UNIQUE,
    user_id TEXT NOT NULL,
    symbol TEXT NOT NULL,
    side TEXT NOT NULL,
    order_type TEXT NOT NULL,
    quantity INTEGER NOT NULL,
    limit_price TEXT,
    state TEXT NOT NULL,
    filled_qty INTEGER NOT NULL DEFAULT 0,
    reject_reason TEXT,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS broker_fills (
    fill_id TEXT PRIMARY KEY,
    broker_order_id TEXT NOT NULL REFERENCES broker_orders(broker_order_id),
    quantity INTEGER NOT NULL,
    price TEXT NOT NULL,
    venue_time TEXT NOT NULL
);
"""


def _to_column(value: Any) -> Any:
    if isinstance(value, bool):
        return int(value)
    if isinstance(value, (list, dict)):
        return json.dumps(value, sort_keys=True)
    return value


class Database:
    def __init__(self, path: str) -> None:
        if path != ":memory:":
            Path(path).parent.mkdir(parents=True, exist_ok=True)
        self._conn = sqlite3.connect(path, check_same_thread=False, isolation_level=None)
        self._conn.row_factory = sqlite3.Row
        self._conn.execute("PRAGMA foreign_keys = ON")
        if path != ":memory:":
            self._conn.execute("PRAGMA journal_mode = WAL")
        self.lock = threading.RLock()
        self._depth = 0
        with self.lock:
            self._conn.executescript(SCHEMA)

    def close(self) -> None:
        with self.lock:
            self._conn.close()

    @contextmanager
    def transaction(self) -> Iterator[None]:
        """Atomic unit of work; nests by joining the outer transaction."""
        with self.lock:
            outermost = self._depth == 0
            if outermost:
                self._conn.execute("BEGIN IMMEDIATE")
            self._depth += 1
            try:
                yield
            except BaseException:
                self._depth -= 1
                if outermost:
                    self._conn.execute("ROLLBACK")
                raise
            else:
                self._depth -= 1
                if outermost:
                    self._conn.execute("COMMIT")

    def execute(self, sql: str, params: tuple | dict = ()) -> sqlite3.Cursor:
        with self.lock:
            return self._conn.execute(sql, params)

    def all(self, sql: str, params: tuple | dict = ()) -> list[dict]:
        with self.lock:
            return [dict(r) for r in self._conn.execute(sql, params).fetchall()]

    def one(self, sql: str, params: tuple | dict = ()) -> dict | None:
        with self.lock:
            row = self._conn.execute(sql, params).fetchone()
            return dict(row) if row else None

    def insert(self, table: str, record: BaseModel | dict, *, or_ignore: bool = False) -> bool:
        data = record.model_dump(mode="json") if isinstance(record, BaseModel) else record
        cols = list(data)
        verb = "INSERT OR IGNORE" if or_ignore else "INSERT"
        sql = f"{verb} INTO {table} ({', '.join(cols)}) VALUES ({', '.join('?' for _ in cols)})"
        cur = self.execute(sql, tuple(_to_column(data[c]) for c in cols))
        return cur.rowcount == 1

    def update(self, table: str, key_col: str, key: str, changes: dict) -> None:
        cols = list(changes)
        sql = f"UPDATE {table} SET {', '.join(f'{c} = ?' for c in cols)} WHERE {key_col} = ?"
        self.execute(sql, (*(_to_column(changes[c]) for c in cols), key))


def load_json_fields(row: dict, *fields: str) -> dict:
    out = dict(row)
    for f in fields:
        out[f] = json.loads(out[f])
    return out
