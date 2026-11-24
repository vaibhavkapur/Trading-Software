"""Typed reads/writes over the state store."""

from __future__ import annotations

from datetime import datetime

from .db import Database, load_json_fields
from .domain import (
    Approval,
    Fill,
    Order,
    ProposalStatus,
    RiskDecision,
    TradeIntent,
    TradeProposal,
)


class Store:
    def __init__(self, db: Database) -> None:
        self.db = db

    def intent(self, intent_id: str) -> TradeIntent | None:
        row = self.db.one("SELECT * FROM intents WHERE id = ?", (intent_id,))
        return TradeIntent.model_validate(row) if row else None

    def proposal(self, proposal_id: str) -> TradeProposal | None:
        row = self.db.one("SELECT * FROM proposals WHERE id = ?", (proposal_id,))
        return TradeProposal.model_validate(row) if row else None

    def set_proposal_status(self, proposal_id: str, status: ProposalStatus, now: datetime) -> None:
        self.db.update(
            "proposals",
            "id",
            proposal_id,
            {"status": status.value, "updated_at": now.isoformat()},
        )

    def similar_live_proposals(self, p: TradeProposal, since: datetime) -> list[TradeProposal]:
        """Other proposals with identical parameters that have, or may get, an order."""
        rows = self.db.all(
            "SELECT * FROM proposals WHERE user_id = ? AND symbol = ? AND side = ? "
            "AND order_type = ? AND quantity = ? AND id != ? AND status IN (?, ?) "
            "AND updated_at >= ? ORDER BY created_at",
            (
                p.user_id,
                p.symbol,
                p.side.value,
                p.order_type.value,
                p.quantity,
                p.id,
                ProposalStatus.APPROVED.value,
                ProposalStatus.EXECUTED.value,
                since.isoformat(),
            ),
        )
        candidates = [TradeProposal.model_validate(r) for r in rows]
        return [c for c in candidates if c.limit_price == p.limit_price]

    def latest_risk_decision(self, proposal_id: str) -> RiskDecision | None:
        row = self.db.one(
            "SELECT * FROM risk_decisions WHERE proposal_id = ? ORDER BY rowid DESC LIMIT 1",
            (proposal_id,),
        )
        if not row:
            return None
        return RiskDecision.model_validate(load_json_fields(row, "violations", "checks"))

    def approval_for(self, proposal_id: str) -> Approval | None:
        row = self.db.one("SELECT * FROM approvals WHERE proposal_id = ?", (proposal_id,))
        return Approval.model_validate(row) if row else None

    def approval(self, approval_id: str) -> Approval | None:
        row = self.db.one("SELECT * FROM approvals WHERE id = ?", (approval_id,))
        return Approval.model_validate(row) if row else None

    def order(self, client_order_id: str) -> Order | None:
        row = self.db.one("SELECT * FROM orders WHERE client_order_id = ?", (client_order_id,))
        return Order.model_validate(row) if row else None

    def order_for_proposal(self, proposal_id: str) -> Order | None:
        row = self.db.one("SELECT * FROM orders WHERE proposal_id = ?", (proposal_id,))
        return Order.model_validate(row) if row else None

    def orders_for_user(self, user_id: str, limit: int = 50) -> list[Order]:
        rows = self.db.all(
            "SELECT * FROM orders WHERE user_id = ? ORDER BY created_at DESC LIMIT ?",
            (user_id, limit),
        )
        return [Order.model_validate(r) for r in rows]

    def fills(self, client_order_id: str) -> list[Fill]:
        rows = self.db.all(
            "SELECT * FROM fills WHERE client_order_id = ? ORDER BY fill_id", (client_order_id,)
        )
        return [Fill.model_validate(r) for r in rows]
