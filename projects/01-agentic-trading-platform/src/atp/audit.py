"""Append-only, hash-chained audit log.

Each event commits to the previous event's hash, so any edit or deletion breaks
`verify_chain`. SQLite triggers additionally reject UPDATE/DELETE on the table.
"""

from __future__ import annotations

import hashlib
import json
from typing import Any

from .clock import Clock
from .db import Database
from .ids import IdFactory

GENESIS_HASH = "0" * 64


def _event_hash(prev_hash: str, body: dict) -> str:
    blob = json.dumps(body, sort_keys=True, separators=(",", ":"), default=str)
    return hashlib.sha256((prev_hash + blob).encode()).hexdigest()


class AuditLog:
    def __init__(self, db: Database, clock: Clock, new_id: IdFactory) -> None:
        self._db = db
        self._clock = clock
        self._new_id = new_id

    def append(
        self, event_type: str, *, correlation_id: str, actor: str, payload: dict[str, Any]
    ) -> dict:
        with self._db.transaction():
            last = self._db.one("SELECT hash FROM audit_events ORDER BY seq DESC LIMIT 1")
            prev_hash = last["hash"] if last else GENESIS_HASH
            body = {
                "id": self._new_id("evt"),
                "ts": self._clock.now().isoformat(),
                "correlation_id": correlation_id,
                "actor": actor,
                "event_type": event_type,
                "payload": json.loads(json.dumps(payload, default=str)),
            }
            record = {**body, "prev_hash": prev_hash, "hash": _event_hash(prev_hash, body)}
            self._db.insert("audit_events", record)
            return record

    def _rows(self, where: str = "", params: tuple = ()) -> list[dict]:
        rows = self._db.all(f"SELECT * FROM audit_events {where} ORDER BY seq", params)
        for row in rows:
            row["payload"] = json.loads(row["payload"])
        return rows

    def by_correlation(self, correlation_id: str) -> list[dict]:
        return self._rows("WHERE correlation_id = ?", (correlation_id,))

    def all(self) -> list[dict]:
        return self._rows()

    def verify_chain(self) -> tuple[bool, int | None]:
        """Return (ok, first_bad_seq)."""
        prev_hash = GENESIS_HASH
        for row in self._rows():
            body = {
                k: row[k] for k in ("id", "ts", "correlation_id", "actor", "event_type", "payload")
            }
            if row["prev_hash"] != prev_hash or row["hash"] != _event_hash(prev_hash, body):
                return False, row["seq"]
            prev_hash = row["hash"]
        return True, None
