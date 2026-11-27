"""SQLite WAL durable state store for OMS, reservations, ledger, reconciliation, and hash-chained audit."""

from __future__ import annotations

import hashlib
import json
import sqlite3
import threading
import uuid
from datetime import datetime
from decimal import Decimal
from pathlib import Path
from typing import Any

from trading_platform.domain.events import (
    AuditEvent,
    ExecutionMode,
    FillEvent,
    JournalLine,
    KillSwitchScope,
    KillSwitchState,
    LedgerEntry,
    OrderRecord,
    OrderState,
    OrderStateTransition,
    OrderType,
    PositionRecord,
    ReconciliationReport,
    RiskReservation,
    Role,
    ShadowDecisionRecord,
    Side,
    TimeInForce,
    utc_now,
)

GENESIS_HASH = "0" * 64


class SQLiteStore:
    """Thread-safe SQLite WAL persistence layer with transactional integrity."""

    def __init__(self, db_path: str | Path = ":memory:") -> None:
        self.db_path = str(db_path)
        if self.db_path != ":memory:":
            Path(self.db_path).parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self._conn = sqlite3.connect(self.db_path, check_same_thread=False)
        self._conn.row_factory = sqlite3.Row
        with self._lock:
            self._conn.execute("PRAGMA journal_mode=WAL;")
            self._conn.execute("PRAGMA foreign_keys=ON;")
            self._init_schema()

    def close(self) -> None:
        with self._lock:
            self._conn.close()

    def _init_schema(self) -> None:
        self._conn.executescript(
            """
            CREATE TABLE IF NOT EXISTS orders (
                client_order_id TEXT PRIMARY KEY,
                idempotency_key TEXT UNIQUE NOT NULL,
                payload_hash TEXT NOT NULL,
                broker_order_id TEXT,
                account_id TEXT NOT NULL,
                strategy_id TEXT NOT NULL,
                signal_id TEXT,
                symbol TEXT NOT NULL,
                side TEXT NOT NULL,
                order_type TEXT NOT NULL,
                time_in_force TEXT NOT NULL,
                quantity TEXT NOT NULL,
                filled_quantity TEXT NOT NULL,
                remaining_quantity TEXT NOT NULL,
                limit_price TEXT,
                reference_price TEXT NOT NULL,
                average_fill_price TEXT,
                currency TEXT NOT NULL,
                state TEXT NOT NULL,
                execution_mode TEXT NOT NULL,
                reject_reason TEXT,
                risk_check_id TEXT,
                created_at_utc TEXT NOT NULL,
                updated_at_utc TEXT NOT NULL
            );

            CREATE INDEX IF NOT EXISTS idx_orders_state ON orders(state);
            CREATE INDEX IF NOT EXISTS idx_orders_symbol ON orders(symbol);

            CREATE TABLE IF NOT EXISTS order_transitions (
                transition_id TEXT PRIMARY KEY,
                client_order_id TEXT NOT NULL,
                from_state TEXT,
                to_state TEXT NOT NULL,
                reason TEXT NOT NULL,
                actor TEXT NOT NULL,
                occurred_at_utc TEXT NOT NULL,
                metadata_json TEXT NOT NULL,
                FOREIGN KEY (client_order_id) REFERENCES orders(client_order_id)
            );

            CREATE TABLE IF NOT EXISTS fills (
                fill_id TEXT PRIMARY KEY,
                client_order_id TEXT NOT NULL,
                broker_order_id TEXT NOT NULL,
                account_id TEXT NOT NULL,
                strategy_id TEXT NOT NULL,
                symbol TEXT NOT NULL,
                side TEXT NOT NULL,
                quantity TEXT NOT NULL,
                price TEXT NOT NULL,
                commission TEXT NOT NULL,
                currency TEXT NOT NULL,
                venue TEXT NOT NULL,
                executed_at_utc TEXT NOT NULL
            );

            CREATE INDEX IF NOT EXISTS idx_fills_order ON fills(client_order_id);

            CREATE TABLE IF NOT EXISTS risk_reservations (
                reservation_id TEXT PRIMARY KEY,
                client_order_id TEXT UNIQUE NOT NULL,
                account_id TEXT NOT NULL,
                strategy_id TEXT NOT NULL,
                symbol TEXT NOT NULL,
                side TEXT NOT NULL,
                reserved_quantity TEXT NOT NULL,
                reserved_notional TEXT NOT NULL,
                currency TEXT NOT NULL,
                active INTEGER NOT NULL,
                retained_reason TEXT,
                created_at_utc TEXT NOT NULL,
                updated_at_utc TEXT NOT NULL
            );

            CREATE INDEX IF NOT EXISTS idx_reservations_active ON risk_reservations(active);

            CREATE TABLE IF NOT EXISTS kill_switches (
                scope TEXT NOT NULL,
                target TEXT NOT NULL,
                engaged INTEGER NOT NULL,
                reason TEXT NOT NULL,
                actor TEXT NOT NULL,
                actor_role TEXT NOT NULL,
                requires_dual_release INTEGER NOT NULL,
                updated_at_utc TEXT NOT NULL,
                PRIMARY KEY (scope, target)
            );

            CREATE TABLE IF NOT EXISTS ledger_entries (
                entry_id TEXT PRIMARY KEY,
                idempotency_key TEXT UNIQUE NOT NULL,
                account_id TEXT NOT NULL,
                entry_type TEXT NOT NULL,
                reference_id TEXT NOT NULL,
                description TEXT NOT NULL,
                posted_at_utc TEXT NOT NULL
            );

            CREATE TABLE IF NOT EXISTS ledger_lines (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                entry_id TEXT NOT NULL,
                account_code TEXT NOT NULL,
                currency TEXT NOT NULL,
                debit TEXT NOT NULL,
                credit TEXT NOT NULL,
                symbol TEXT,
                memo TEXT NOT NULL,
                FOREIGN KEY (entry_id) REFERENCES ledger_entries(entry_id)
            );

            CREATE TABLE IF NOT EXISTS positions (
                account_id TEXT NOT NULL,
                symbol TEXT NOT NULL,
                currency TEXT NOT NULL,
                quantity TEXT NOT NULL,
                average_cost TEXT NOT NULL,
                realized_pnl TEXT NOT NULL,
                total_commissions TEXT NOT NULL,
                updated_at_utc TEXT NOT NULL,
                PRIMARY KEY (account_id, symbol)
            );

            CREATE TABLE IF NOT EXISTS reconciliation_reports (
                reconciliation_id TEXT PRIMARY KEY,
                account_id TEXT NOT NULL,
                run_at_utc TEXT NOT NULL,
                matched INTEGER NOT NULL,
                report_json TEXT NOT NULL
            );

            CREATE TABLE IF NOT EXISTS shadow_decisions (
                shadow_id TEXT PRIMARY KEY,
                tick_id TEXT NOT NULL,
                strategy_id TEXT NOT NULL,
                symbol TEXT NOT NULL,
                record_json TEXT NOT NULL,
                recorded_at_utc TEXT NOT NULL
            );

            CREATE TABLE IF NOT EXISTS audit_events (
                sequence_no INTEGER PRIMARY KEY AUTOINCREMENT,
                event_id TEXT UNIQUE NOT NULL,
                event_type TEXT NOT NULL,
                actor TEXT NOT NULL,
                actor_role TEXT NOT NULL,
                entity_type TEXT NOT NULL,
                entity_id TEXT NOT NULL,
                payload_json TEXT NOT NULL,
                occurred_at_utc TEXT NOT NULL,
                prev_hash TEXT NOT NULL,
                event_hash TEXT NOT NULL
            );
            """
        )
        self._conn.commit()

    # --- Orders & Transitions ---

    def save_order(self, order: OrderRecord) -> None:
        with self._lock:
            self._conn.execute(
                """
                INSERT INTO orders (
                    client_order_id, idempotency_key, payload_hash, broker_order_id,
                    account_id, strategy_id, signal_id, symbol, side, order_type,
                    time_in_force, quantity, filled_quantity, remaining_quantity,
                    limit_price, reference_price, average_fill_price, currency,
                    state, execution_mode, reject_reason, risk_check_id,
                    created_at_utc, updated_at_utc
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(client_order_id) DO UPDATE SET
                    broker_order_id=excluded.broker_order_id,
                    filled_quantity=excluded.filled_quantity,
                    remaining_quantity=excluded.remaining_quantity,
                    average_fill_price=excluded.average_fill_price,
                    state=excluded.state,
                    reject_reason=excluded.reject_reason,
                    risk_check_id=excluded.risk_check_id,
                    updated_at_utc=excluded.updated_at_utc
                """,
                (
                    order.client_order_id,
                    order.idempotency_key,
                    order.payload_hash,
                    order.broker_order_id,
                    order.account_id,
                    order.strategy_id,
                    order.signal_id,
                    order.symbol,
                    order.side.value,
                    order.order_type.value,
                    order.time_in_force.value,
                    str(order.quantity),
                    str(order.filled_quantity),
                    str(order.remaining_quantity),
                    str(order.limit_price) if order.limit_price is not None else None,
                    str(order.reference_price),
                    str(order.average_fill_price) if order.average_fill_price is not None else None,
                    order.currency,
                    order.state.value,
                    order.execution_mode.value,
                    order.reject_reason,
                    order.risk_check_id,
                    order.created_at_utc.isoformat(),
                    order.updated_at_utc.isoformat(),
                ),
            )
            self._conn.commit()

    def _row_to_order(self, row: sqlite3.Row) -> OrderRecord:
        return OrderRecord(
            client_order_id=row["client_order_id"],
            idempotency_key=row["idempotency_key"],
            payload_hash=row["payload_hash"],
            broker_order_id=row["broker_order_id"],
            account_id=row["account_id"],
            strategy_id=row["strategy_id"],
            signal_id=row["signal_id"],
            symbol=row["symbol"],
            side=Side(row["side"]),
            order_type=OrderType(row["order_type"]),
            time_in_force=TimeInForce(row["time_in_force"]),
            quantity=Decimal(row["quantity"]),
            filled_quantity=Decimal(row["filled_quantity"]),
            remaining_quantity=Decimal(row["remaining_quantity"]),
            limit_price=Decimal(row["limit_price"]) if row["limit_price"] is not None else None,
            reference_price=Decimal(row["reference_price"]),
            average_fill_price=(
                Decimal(row["average_fill_price"])
                if row["average_fill_price"] is not None
                else None
            ),
            currency=row["currency"],
            state=OrderState(row["state"]),
            execution_mode=ExecutionMode(row["execution_mode"]),
            reject_reason=row["reject_reason"],
            risk_check_id=row["risk_check_id"],
            created_at_utc=datetime.fromisoformat(row["created_at_utc"]),
            updated_at_utc=datetime.fromisoformat(row["updated_at_utc"]),
        )

    def get_order(self, client_order_id: str) -> OrderRecord | None:
        with self._lock:
            row = self._conn.execute(
                "SELECT * FROM orders WHERE client_order_id = ?", (client_order_id,)
            ).fetchone()
            return self._row_to_order(row) if row else None

    def get_order_by_idempotency_key(self, idempotency_key: str) -> OrderRecord | None:
        with self._lock:
            row = self._conn.execute(
                "SELECT * FROM orders WHERE idempotency_key = ?", (idempotency_key,)
            ).fetchone()
            return self._row_to_order(row) if row else None

    def list_orders(self, *, states: set[OrderState] | None = None) -> list[OrderRecord]:
        with self._lock:
            if states:
                placeholders = ",".join("?" for _ in states)
                rows = self._conn.execute(
                    f"SELECT * FROM orders WHERE state IN ({placeholders}) ORDER BY created_at_utc ASC",
                    tuple(s.value for s in states),
                ).fetchall()
            else:
                rows = self._conn.execute(
                    "SELECT * FROM orders ORDER BY created_at_utc ASC"
                ).fetchall()
            return [self._row_to_order(r) for r in rows]

    def save_transition(self, transition: OrderStateTransition) -> None:
        with self._lock:
            self._conn.execute(
                """
                INSERT INTO order_transitions (
                    transition_id, client_order_id, from_state, to_state,
                    reason, actor, occurred_at_utc, metadata_json
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    transition.transition_id,
                    transition.client_order_id,
                    transition.from_state.value if transition.from_state else None,
                    transition.to_state.value,
                    transition.reason,
                    transition.actor,
                    transition.occurred_at_utc.isoformat(),
                    json.dumps(transition.metadata, sort_keys=True, default=str),
                ),
            )
            self._conn.commit()

    def list_transitions(self, client_order_id: str) -> list[OrderStateTransition]:
        with self._lock:
            rows = self._conn.execute(
                "SELECT * FROM order_transitions WHERE client_order_id = ? ORDER BY rowid ASC",
                (client_order_id,),
            ).fetchall()
            return [
                OrderStateTransition(
                    transition_id=r["transition_id"],
                    client_order_id=r["client_order_id"],
                    from_state=OrderState(r["from_state"]) if r["from_state"] else None,
                    to_state=OrderState(r["to_state"]),
                    reason=r["reason"],
                    actor=r["actor"],
                    occurred_at_utc=datetime.fromisoformat(r["occurred_at_utc"]),
                    metadata=json.loads(r["metadata_json"]),
                )
                for r in rows
            ]

    # --- Fills ---

    def save_fill_if_absent(self, fill: FillEvent) -> bool:
        """Returns True if inserted, False if fill_id already exists (idempotent)."""
        with self._lock:
            cur = self._conn.execute(
                """
                INSERT OR IGNORE INTO fills (
                    fill_id, client_order_id, broker_order_id, account_id,
                    strategy_id, symbol, side, quantity, price, commission,
                    currency, venue, executed_at_utc
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    fill.fill_id,
                    fill.client_order_id,
                    fill.broker_order_id,
                    fill.account_id,
                    fill.strategy_id,
                    fill.symbol,
                    fill.side.value,
                    str(fill.quantity),
                    str(fill.price),
                    str(fill.commission),
                    fill.currency,
                    fill.venue,
                    fill.executed_at_utc.isoformat(),
                ),
            )
            self._conn.commit()
            return cur.rowcount == 1

    def list_fills(self, client_order_id: str | None = None) -> list[FillEvent]:
        with self._lock:
            if client_order_id:
                rows = self._conn.execute(
                    "SELECT * FROM fills WHERE client_order_id = ? ORDER BY executed_at_utc ASC",
                    (client_order_id,),
                ).fetchall()
            else:
                rows = self._conn.execute(
                    "SELECT * FROM fills ORDER BY executed_at_utc ASC"
                ).fetchall()
            return [
                FillEvent(
                    fill_id=r["fill_id"],
                    client_order_id=r["client_order_id"],
                    broker_order_id=r["broker_order_id"],
                    account_id=r["account_id"],
                    strategy_id=r["strategy_id"],
                    symbol=r["symbol"],
                    side=Side(r["side"]),
                    quantity=Decimal(r["quantity"]),
                    price=Decimal(r["price"]),
                    commission=Decimal(r["commission"]),
                    currency=r["currency"],
                    venue=r["venue"],
                    executed_at_utc=datetime.fromisoformat(r["executed_at_utc"]),
                )
                for r in rows
            ]

    # --- Risk Reservations ---

    def save_reservation(self, res: RiskReservation) -> None:
        with self._lock:
            self._conn.execute(
                """
                INSERT INTO risk_reservations (
                    reservation_id, client_order_id, account_id, strategy_id,
                    symbol, side, reserved_quantity, reserved_notional, currency,
                    active, retained_reason, created_at_utc, updated_at_utc
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(client_order_id) DO UPDATE SET
                    reserved_quantity=excluded.reserved_quantity,
                    reserved_notional=excluded.reserved_notional,
                    active=excluded.active,
                    retained_reason=excluded.retained_reason,
                    updated_at_utc=excluded.updated_at_utc
                """,
                (
                    res.reservation_id,
                    res.client_order_id,
                    res.account_id,
                    res.strategy_id,
                    res.symbol,
                    res.side.value,
                    str(res.reserved_quantity),
                    str(res.reserved_notional),
                    res.currency,
                    1 if res.active else 0,
                    res.retained_reason,
                    res.created_at_utc.isoformat(),
                    res.updated_at_utc.isoformat(),
                ),
            )
            self._conn.commit()

    def get_reservation(self, client_order_id: str) -> RiskReservation | None:
        with self._lock:
            row = self._conn.execute(
                "SELECT * FROM risk_reservations WHERE client_order_id = ?",
                (client_order_id,),
            ).fetchone()
            return self._row_to_reservation(row) if row else None

    def list_reservations(self, *, active_only: bool = True) -> list[RiskReservation]:
        with self._lock:
            if active_only:
                rows = self._conn.execute(
                    "SELECT * FROM risk_reservations WHERE active = 1 ORDER BY created_at_utc ASC"
                ).fetchall()
            else:
                rows = self._conn.execute(
                    "SELECT * FROM risk_reservations ORDER BY created_at_utc ASC"
                ).fetchall()
            return [self._row_to_reservation(r) for r in rows]

    def _row_to_reservation(self, row: sqlite3.Row) -> RiskReservation:
        return RiskReservation(
            reservation_id=row["reservation_id"],
            client_order_id=row["client_order_id"],
            account_id=row["account_id"],
            strategy_id=row["strategy_id"],
            symbol=row["symbol"],
            side=Side(row["side"]),
            reserved_quantity=Decimal(row["reserved_quantity"]),
            reserved_notional=Decimal(row["reserved_notional"]),
            currency=row["currency"],
            active=bool(row["active"]),
            retained_reason=row["retained_reason"],
            created_at_utc=datetime.fromisoformat(row["created_at_utc"]),
            updated_at_utc=datetime.fromisoformat(row["updated_at_utc"]),
        )

    # --- Kill Switch ---

    def save_kill_switch(self, state: KillSwitchState) -> None:
        with self._lock:
            self._conn.execute(
                """
                INSERT INTO kill_switches (
                    scope, target, engaged, reason, actor, actor_role,
                    requires_dual_release, updated_at_utc
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(scope, target) DO UPDATE SET
                    engaged=excluded.engaged,
                    reason=excluded.reason,
                    actor=excluded.actor,
                    actor_role=excluded.actor_role,
                    requires_dual_release=excluded.requires_dual_release,
                    updated_at_utc=excluded.updated_at_utc
                """,
                (
                    state.scope.value,
                    state.target,
                    1 if state.engaged else 0,
                    state.reason,
                    state.actor,
                    state.actor_role.value,
                    1 if state.requires_dual_release else 0,
                    state.updated_at_utc.isoformat(),
                ),
            )
            self._conn.commit()

    def list_kill_switches(self, *, engaged_only: bool = False) -> list[KillSwitchState]:
        with self._lock:
            if engaged_only:
                rows = self._conn.execute(
                    "SELECT * FROM kill_switches WHERE engaged = 1 ORDER BY scope, target"
                ).fetchall()
            else:
                rows = self._conn.execute(
                    "SELECT * FROM kill_switches ORDER BY scope, target"
                ).fetchall()
            return [
                KillSwitchState(
                    scope=KillSwitchScope(r["scope"]),
                    target=r["target"],
                    engaged=bool(r["engaged"]),
                    reason=r["reason"],
                    actor=r["actor"],
                    actor_role=Role(r["actor_role"]),
                    requires_dual_release=bool(r["requires_dual_release"]),
                    updated_at_utc=datetime.fromisoformat(r["updated_at_utc"]),
                )
                for r in rows
            ]

    # --- Ledger & Positions ---

    def save_ledger_entry_if_absent(self, entry: LedgerEntry) -> bool:
        with self._lock:
            existing = self._conn.execute(
                "SELECT 1 FROM ledger_entries WHERE idempotency_key = ?",
                (entry.idempotency_key,),
            ).fetchone()
            if existing:
                return False
            self._conn.execute(
                """
                INSERT INTO ledger_entries (
                    entry_id, idempotency_key, account_id, entry_type,
                    reference_id, description, posted_at_utc
                ) VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    entry.entry_id,
                    entry.idempotency_key,
                    entry.account_id,
                    entry.entry_type,
                    entry.reference_id,
                    entry.description,
                    entry.posted_at_utc.isoformat(),
                ),
            )
            for line in entry.lines:
                self._conn.execute(
                    """
                    INSERT INTO ledger_lines (
                        entry_id, account_code, currency, debit, credit, symbol, memo
                    ) VALUES (?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        entry.entry_id,
                        line.account_code,
                        line.currency,
                        str(line.debit),
                        str(line.credit),
                        line.symbol,
                        line.memo,
                    ),
                )
            self._conn.commit()
            return True

    def list_ledger_entries(self) -> list[LedgerEntry]:
        with self._lock:
            entry_rows = self._conn.execute(
                "SELECT * FROM ledger_entries ORDER BY posted_at_utc ASC, rowid ASC"
            ).fetchall()
            entries: list[LedgerEntry] = []
            for er in entry_rows:
                line_rows = self._conn.execute(
                    "SELECT * FROM ledger_lines WHERE entry_id = ? ORDER BY id ASC",
                    (er["entry_id"],),
                ).fetchall()
                lines = [
                    JournalLine(
                        account_code=lr["account_code"],
                        currency=lr["currency"],
                        debit=Decimal(lr["debit"]),
                        credit=Decimal(lr["credit"]),
                        symbol=lr["symbol"],
                        memo=lr["memo"],
                    )
                    for lr in line_rows
                ]
                entries.append(
                    LedgerEntry(
                        entry_id=er["entry_id"],
                        idempotency_key=er["idempotency_key"],
                        account_id=er["account_id"],
                        entry_type=er["entry_type"],
                        reference_id=er["reference_id"],
                        description=er["description"],
                        posted_at_utc=datetime.fromisoformat(er["posted_at_utc"]),
                        lines=lines,
                    )
                )
            return entries

    def save_position(self, pos: PositionRecord) -> None:
        with self._lock:
            self._conn.execute(
                """
                INSERT INTO positions (
                    account_id, symbol, currency, quantity, average_cost,
                    realized_pnl, total_commissions, updated_at_utc
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(account_id, symbol) DO UPDATE SET
                    quantity=excluded.quantity,
                    average_cost=excluded.average_cost,
                    realized_pnl=excluded.realized_pnl,
                    total_commissions=excluded.total_commissions,
                    updated_at_utc=excluded.updated_at_utc
                """,
                (
                    pos.account_id,
                    pos.symbol,
                    pos.currency,
                    str(pos.quantity),
                    str(pos.average_cost),
                    str(pos.realized_pnl),
                    str(pos.total_commissions),
                    pos.updated_at_utc.isoformat(),
                ),
            )
            self._conn.commit()

    def get_position(self, account_id: str, symbol: str) -> PositionRecord | None:
        with self._lock:
            row = self._conn.execute(
                "SELECT * FROM positions WHERE account_id = ? AND symbol = ?",
                (account_id, symbol),
            ).fetchone()
            return self._row_to_position(row) if row else None

    def list_positions(self, account_id: str | None = None) -> list[PositionRecord]:
        with self._lock:
            if account_id:
                rows = self._conn.execute(
                    "SELECT * FROM positions WHERE account_id = ? ORDER BY symbol ASC",
                    (account_id,),
                ).fetchall()
            else:
                rows = self._conn.execute(
                    "SELECT * FROM positions ORDER BY account_id, symbol ASC"
                ).fetchall()
            return [self._row_to_position(r) for r in rows]

    def _row_to_position(self, row: sqlite3.Row) -> PositionRecord:
        return PositionRecord(
            account_id=row["account_id"],
            symbol=row["symbol"],
            currency=row["currency"],
            quantity=Decimal(row["quantity"]),
            average_cost=Decimal(row["average_cost"]),
            realized_pnl=Decimal(row["realized_pnl"]),
            total_commissions=Decimal(row["total_commissions"]),
            updated_at_utc=datetime.fromisoformat(row["updated_at_utc"]),
        )

    # --- Reconciliation & Shadow ---

    def save_reconciliation_report(self, report: ReconciliationReport) -> None:
        with self._lock:
            self._conn.execute(
                """
                INSERT INTO reconciliation_reports (
                    reconciliation_id, account_id, run_at_utc, matched, report_json
                ) VALUES (?, ?, ?, ?, ?)
                """,
                (
                    report.reconciliation_id,
                    report.account_id,
                    report.run_at_utc.isoformat(),
                    1 if report.matched else 0,
                    report.model_dump_json(),
                ),
            )
            self._conn.commit()

    def list_reconciliation_reports(self) -> list[ReconciliationReport]:
        with self._lock:
            rows = self._conn.execute(
                "SELECT report_json FROM reconciliation_reports ORDER BY run_at_utc ASC"
            ).fetchall()
            return [ReconciliationReport.model_validate_json(r["report_json"]) for r in rows]

    def save_shadow_decision(self, record: ShadowDecisionRecord) -> None:
        with self._lock:
            self._conn.execute(
                """
                INSERT INTO shadow_decisions (
                    shadow_id, tick_id, strategy_id, symbol, record_json, recorded_at_utc
                ) VALUES (?, ?, ?, ?, ?, ?)
                """,
                (
                    record.shadow_id,
                    record.tick_id,
                    record.strategy_id,
                    record.symbol,
                    record.model_dump_json(),
                    record.recorded_at_utc.isoformat(),
                ),
            )
            self._conn.commit()

    def list_shadow_decisions(self) -> list[ShadowDecisionRecord]:
        with self._lock:
            rows = self._conn.execute(
                "SELECT record_json FROM shadow_decisions ORDER BY recorded_at_utc ASC"
            ).fetchall()
            return [ShadowDecisionRecord.model_validate_json(r["record_json"]) for r in rows]

    # --- Hash-Chained Audit Log ---

    def append_audit(
        self,
        *,
        event_type: str,
        actor: str,
        actor_role: Role,
        entity_type: str,
        entity_id: str,
        payload: dict[str, Any] | None = None,
        occurred_at_utc: datetime | None = None,
    ) -> AuditEvent:
        with self._lock:
            last = self._conn.execute(
                "SELECT sequence_no, event_hash FROM audit_events ORDER BY sequence_no DESC LIMIT 1"
            ).fetchone()
            prev_hash = last["event_hash"] if last else GENESIS_HASH
            next_seq = (int(last["sequence_no"]) + 1) if last else 1
            event_id = f"AUD-{uuid.uuid4().hex[:12].upper()}"
            ts = occurred_at_utc or utc_now()
            body = payload or {}
            payload_json = json.dumps(body, sort_keys=True, separators=(",", ":"), default=str)
            digest_input = f"{next_seq}|{event_id}|{event_type}|{actor}|{actor_role.value}|{entity_type}|{entity_id}|{ts.isoformat()}|{payload_json}|{prev_hash}"
            event_hash = hashlib.sha256(digest_input.encode("utf-8")).hexdigest()

            self._conn.execute(
                """
                INSERT INTO audit_events (
                    sequence_no, event_id, event_type, actor, actor_role,
                    entity_type, entity_id, payload_json, occurred_at_utc,
                    prev_hash, event_hash
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    next_seq,
                    event_id,
                    event_type,
                    actor,
                    actor_role.value,
                    entity_type,
                    entity_id,
                    payload_json,
                    ts.isoformat(),
                    prev_hash,
                    event_hash,
                ),
            )
            self._conn.commit()
            return AuditEvent(
                sequence_no=next_seq,
                event_id=event_id,
                event_type=event_type,
                actor=actor,
                actor_role=actor_role,
                entity_type=entity_type,
                entity_id=entity_id,
                payload=body,
                occurred_at_utc=ts,
                prev_hash=prev_hash,
                event_hash=event_hash,
            )

    def list_audit_events(self, limit: int | None = None) -> list[AuditEvent]:
        with self._lock:
            if limit is not None:
                rows = self._conn.execute(
                    "SELECT * FROM audit_events ORDER BY sequence_no DESC LIMIT ?",
                    (limit,),
                ).fetchall()
                rows = list(reversed(rows))
            else:
                rows = self._conn.execute(
                    "SELECT * FROM audit_events ORDER BY sequence_no ASC"
                ).fetchall()
            return [
                AuditEvent(
                    sequence_no=r["sequence_no"],
                    event_id=r["event_id"],
                    event_type=r["event_type"],
                    actor=r["actor"],
                    actor_role=Role(r["actor_role"]),
                    entity_type=r["entity_type"],
                    entity_id=r["entity_id"],
                    payload=json.loads(r["payload_json"]),
                    occurred_at_utc=datetime.fromisoformat(r["occurred_at_utc"]),
                    prev_hash=r["prev_hash"],
                    event_hash=r["event_hash"],
                )
                for r in rows
            ]

    def verify_audit_chain(self) -> tuple[bool, str]:
        events = self.list_audit_events()
        expected_prev = GENESIS_HASH
        for ev in events:
            if ev.prev_hash != expected_prev:
                return False, f"Broken prev_hash at sequence {ev.sequence_no}"
            payload_json = json.dumps(
                ev.payload, sort_keys=True, separators=(",", ":"), default=str
            )
            digest_input = f"{ev.sequence_no}|{ev.event_id}|{ev.event_type}|{ev.actor}|{ev.actor_role.value}|{ev.entity_type}|{ev.entity_id}|{ev.occurred_at_utc.isoformat()}|{payload_json}|{ev.prev_hash}"
            recomputed = hashlib.sha256(digest_input.encode("utf-8")).hexdigest()
            if recomputed != ev.event_hash:
                return False, f"Tampered event_hash at sequence {ev.sequence_no}"
            expected_prev = ev.event_hash
        return True, f"Verified {len(events)} hash-chained audit events"
