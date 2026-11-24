"""SQLite-backed state store for paper multi-leg orders and simulated positions."""

from __future__ import annotations

import json
import sqlite3
from decimal import Decimal
from pathlib import Path

from options_infra.domain.models import MultiLegOrder, OptionPosition, OrderSide


class StoreWriteError(RuntimeError):
    """Raised when persisting an order or position fails (e.g., disk/DB write rejection)."""


class PaperOrderStore:
    """Durable SQLite store supporting restart recovery, idempotency, and failure injection."""

    def __init__(self, db_path: str | Path = ":memory:") -> None:
        self._db_path = str(db_path)
        if self._db_path != ":memory:":
            Path(self._db_path).parent.mkdir(parents=True, exist_ok=True)
        self._conn = sqlite3.connect(self._db_path, check_same_thread=False)
        self._conn.row_factory = sqlite3.Row
        self._simulate_write_failure = False
        self._init_schema()

    def _init_schema(self) -> None:
        with self._conn:
            self._conn.execute(
                """
                CREATE TABLE IF NOT EXISTS paper_orders (
                    id TEXT PRIMARY KEY,
                    idempotency_key TEXT UNIQUE,
                    status TEXT NOT NULL,
                    payload_json TEXT NOT NULL,
                    created_at TEXT NOT NULL,
                    updated_at TEXT NOT NULL
                )
                """
            )
            self._conn.execute(
                """
                CREATE TABLE IF NOT EXISTS paper_positions (
                    account_id TEXT NOT NULL,
                    contract_id TEXT NOT NULL,
                    quantity INTEGER NOT NULL,
                    avg_price TEXT NOT NULL,
                    PRIMARY KEY (account_id, contract_id)
                )
                """
            )

    def set_simulate_write_failure(self, enabled: bool) -> None:
        """Inject persistent write rejection for scenario testing."""
        self._simulate_write_failure = enabled

    def close(self) -> None:
        self._conn.close()

    def save_order(self, order: MultiLegOrder) -> None:
        if self._simulate_write_failure:
            raise StoreWriteError("Simulated storage write failure: persistence rejected")
        payload = order.model_dump_json()
        try:
            with self._conn:
                self._conn.execute(
                    """
                    INSERT INTO paper_orders (
                        id, idempotency_key, status, payload_json, created_at, updated_at
                    )
                    VALUES (?, ?, ?, ?, ?, ?)
                    ON CONFLICT(id) DO UPDATE SET
                        status = excluded.status,
                        payload_json = excluded.payload_json,
                        updated_at = excluded.updated_at
                    """,
                    (
                        order.id,
                        order.idempotency_key,
                        order.status.value,
                        payload,
                        order.created_at.isoformat(),
                        order.updated_at.isoformat(),
                    ),
                )
        except sqlite3.Error as exc:
            raise StoreWriteError(f"SQLite write failed: {exc}") from exc

    def get_order(self, order_id: str) -> MultiLegOrder | None:
        row = self._conn.execute(
            "SELECT payload_json FROM paper_orders WHERE id = ?",
            (order_id,),
        ).fetchone()
        if row is None:
            return None
        return MultiLegOrder.model_validate_json(row["payload_json"])

    def get_order_by_idempotency_key(self, idempotency_key: str) -> MultiLegOrder | None:
        row = self._conn.execute(
            "SELECT payload_json FROM paper_orders WHERE idempotency_key = ?",
            (idempotency_key,),
        ).fetchone()
        if row is None:
            return None
        return MultiLegOrder.model_validate_json(row["payload_json"])

    def list_orders(self, limit: int = 100) -> list[MultiLegOrder]:
        rows = self._conn.execute(
            "SELECT payload_json FROM paper_orders ORDER BY created_at DESC LIMIT ?",
            (limit,),
        ).fetchall()
        return [MultiLegOrder.model_validate_json(r["payload_json"]) for r in rows]

    def apply_filled_order_to_positions(
        self,
        order: MultiLegOrder,
        leg_fill_prices: dict[str, Decimal],
    ) -> None:
        """Update paper positions atomically using fixed-point Decimal arithmetic."""
        if self._simulate_write_failure:
            raise StoreWriteError("Simulated storage write failure during position update")

        try:
            with self._conn:
                for leg in order.legs:
                    signed_delta = (
                        leg.ratio * order.quantity
                        if leg.side == OrderSide.BUY
                        else -(leg.ratio * order.quantity)
                    )
                    fill_px = leg_fill_prices[leg.contract_id]
                    row = self._conn.execute(
                        """
                        SELECT quantity, avg_price
                        FROM paper_positions
                        WHERE account_id = ? AND contract_id = ?
                        """,
                        (order.account_id, leg.contract_id),
                    ).fetchone()

                    if row is None:
                        new_qty = signed_delta
                        new_avg = fill_px
                    else:
                        old_qty = int(row["quantity"])
                        old_avg = Decimal(row["avg_price"])
                        new_qty = old_qty + signed_delta
                        if new_qty == 0:
                            self._conn.execute(
                                """
                                DELETE FROM paper_positions
                                WHERE account_id = ? AND contract_id = ?
                                """,
                                (order.account_id, leg.contract_id),
                            )
                            continue
                        # Same direction adds to weighted average; flipping resets avg to fill
                        if (old_qty > 0 and signed_delta > 0) or (
                            old_qty < 0 and signed_delta < 0
                        ):
                            total_cost = (abs(old_qty) * old_avg) + (abs(signed_delta) * fill_px)
                            new_avg = (total_cost / Decimal(abs(new_qty))).quantize(
                                Decimal("0.0001")
                            )
                        elif (old_qty > 0 > new_qty) or (old_qty < 0 < new_qty):
                            new_avg = fill_px
                        else:
                            new_avg = old_avg

                    self._conn.execute(
                        """
                        INSERT INTO paper_positions (account_id, contract_id, quantity, avg_price)
                        VALUES (?, ?, ?, ?)
                        ON CONFLICT(account_id, contract_id) DO UPDATE SET
                            quantity = excluded.quantity,
                            avg_price = excluded.avg_price
                        """,
                        (order.account_id, leg.contract_id, new_qty, str(new_avg)),
                    )
        except sqlite3.Error as exc:
            raise StoreWriteError(f"SQLite position update failed: {exc}") from exc

    def list_positions(self, account_id: str = "PAPER-DEFAULT") -> list[OptionPosition]:
        rows = self._conn.execute(
            """
            SELECT account_id, contract_id, quantity, avg_price
            FROM paper_positions
            WHERE account_id = ?
            ORDER BY contract_id
            """,
            (account_id,),
        ).fetchall()
        return [
            OptionPosition(
                account_id=r["account_id"],
                contract_id=r["contract_id"],
                quantity=int(r["quantity"]),
                avg_price=Decimal(r["avg_price"]),
            )
            for r in rows
        ]

    def export_state(self) -> str:
        """Export deterministic JSON snapshot of orders and positions."""
        return json.dumps(
            {
                "orders": [o.model_dump(mode="json") for o in self.list_orders(limit=1000)],
                "positions": [p.model_dump(mode="json") for p in self.list_positions()],
            },
            indent=2,
        )
