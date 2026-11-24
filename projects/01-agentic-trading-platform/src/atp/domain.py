"""Domain model. Money and prices are Decimal; quantities are whole shares (int)."""

from __future__ import annotations

import hashlib
import json
from datetime import datetime
from decimal import Decimal
from enum import StrEnum

from pydantic import BaseModel, Field

TICK = Decimal("0.01")
CENT = Decimal("0.01")


class Side(StrEnum):
    BUY = "buy"
    SELL = "sell"


class OrderType(StrEnum):
    LIMIT = "limit"
    MARKET = "market"


class ProposalStatus(StrEnum):
    DRAFT = "draft"
    RISK_PASSED = "risk_passed"
    RISK_REJECTED = "risk_rejected"
    APPROVED = "approved"
    REJECTED = "rejected"  # rejected by the human approver
    EXECUTED = "executed"
    FAILED = "failed"  # approved, but the order was rejected or canceled unfilled


class Decision(StrEnum):
    APPROVE = "approve"
    REJECT = "reject"


class OrderState(StrEnum):
    CREATED = "created"
    SUBMITTING = "submitting"  # dispatched or dispatch outcome unknown
    ACCEPTED = "accepted"
    PARTIALLY_FILLED = "partially_filled"
    FILLED = "filled"
    CANCELED = "canceled"
    REJECTED = "rejected"


TERMINAL_ORDER_STATES = frozenset({OrderState.FILLED, OrderState.CANCELED, OrderState.REJECTED})

ORDER_TRANSITIONS: dict[OrderState, frozenset[OrderState]] = {
    OrderState.CREATED: frozenset(
        {OrderState.SUBMITTING, OrderState.REJECTED, OrderState.CANCELED}
    ),
    OrderState.SUBMITTING: frozenset(
        {
            OrderState.ACCEPTED,
            OrderState.PARTIALLY_FILLED,
            OrderState.FILLED,
            OrderState.REJECTED,
            OrderState.CANCELED,
        }
    ),
    OrderState.ACCEPTED: frozenset(
        {
            OrderState.PARTIALLY_FILLED,
            OrderState.FILLED,
            OrderState.CANCELED,
            OrderState.REJECTED,
        }
    ),
    OrderState.PARTIALLY_FILLED: frozenset({OrderState.FILLED, OrderState.CANCELED}),
    OrderState.FILLED: frozenset(),
    OrderState.CANCELED: frozenset(),
    OrderState.REJECTED: frozenset(),
}


class IllegalTransition(Exception):
    pass


def check_transition(current: OrderState, new: OrderState) -> None:
    if current != new and new not in ORDER_TRANSITIONS[current]:
        raise IllegalTransition(f"order cannot move from {current.value} to {new.value}")


class Quote(BaseModel):
    symbol: str
    bid: Decimal
    ask: Decimal
    last: Decimal
    event_time: datetime  # venue/event time
    received_at: datetime  # local receive time

    @property
    def mid(self) -> Decimal:
        return (self.bid + self.ask) / 2


class ParsedIntent(BaseModel):
    """What a planner (LLM or rules) may propose. Nothing here is trusted downstream."""

    actionable: bool
    symbol: str | None = None
    side: Side | None = None
    notional: str | None = None  # dollars, as a decimal string
    quantity: int | None = None  # whole shares
    order_type: OrderType | None = None
    limit_price: str | None = None  # decimal string
    rationale: str = ""
    clarification: str | None = None  # why it is not actionable


class TradeIntent(BaseModel):
    id: str
    correlation_id: str
    user_id: str
    text: str
    mode: str
    symbol: str
    side: Side
    notional: Decimal | None
    quantity: int | None
    order_type: OrderType
    limit_price: Decimal | None
    rationale: str
    planner: str
    created_at: datetime


class TradeProposal(BaseModel):
    id: str
    intent_id: str
    correlation_id: str
    user_id: str
    mode: str
    symbol: str
    side: Side
    order_type: OrderType
    quantity: int
    limit_price: Decimal | None
    time_in_force: str = "day"
    reference_price: Decimal  # quote price used for sizing; informational
    quote_asof: datetime  # venue time of the sizing quote
    quote_received_at: datetime
    status: ProposalStatus
    proposal_hash: str
    created_at: datetime
    updated_at: datetime


class CheckResult(BaseModel):
    code: str
    passed: bool
    detail: str


class RiskDecision(BaseModel):
    id: str
    proposal_id: str
    policy_version: str
    stage: str  # "pre_approval" or "pre_submit"
    passed: bool
    violations: list[CheckResult]
    checks: list[CheckResult]
    evaluated_at: datetime


class Approval(BaseModel):
    id: str
    proposal_id: str
    proposal_hash: str
    approver_id: str
    decision: Decision
    created_at: datetime
    expires_at: datetime
    signature: str


class Order(BaseModel):
    client_order_id: str
    proposal_id: str
    approval_id: str
    correlation_id: str
    user_id: str
    symbol: str
    side: Side
    order_type: OrderType
    quantity: int
    limit_price: Decimal | None
    state: OrderState
    broker_order_id: str | None = None
    filled_qty: int = 0
    avg_fill_price: Decimal | None = None
    submit_attempts: int = 0
    reject_reason: str | None = None
    created_at: datetime
    updated_at: datetime


class Fill(BaseModel):
    fill_id: str
    client_order_id: str
    quantity: int
    price: Decimal
    venue_time: datetime
    received_at: datetime | None = None


class Position(BaseModel):
    symbol: str
    quantity: int
    avg_cost: Decimal


class AccountSnapshot(BaseModel):
    user_id: str
    cash: Decimal
    positions: list[Position] = Field(default_factory=list)

    def position_qty(self, symbol: str) -> int:
        return next((p.quantity for p in self.positions if p.symbol == symbol), 0)


def canonical_decimal(value: Decimal | None) -> str | None:
    if value is None:
        return None
    return format(value.normalize(), "f")


def compute_proposal_hash(
    *,
    proposal_id: str,
    intent_id: str,
    user_id: str,
    mode: str,
    symbol: str,
    side: Side | str,
    order_type: OrderType | str,
    quantity: int,
    limit_price: Decimal | None,
    time_in_force: str,
) -> str:
    """Hash of every parameter that determines what would execute.

    An approval is bound to this hash, so changing any field invalidates it.
    """
    material = {
        "proposal_id": proposal_id,
        "intent_id": intent_id,
        "user_id": user_id,
        "mode": mode,
        "symbol": symbol,
        "side": str(Side(side).value),
        "order_type": str(OrderType(order_type).value),
        "quantity": int(quantity),
        "limit_price": canonical_decimal(limit_price),
        "time_in_force": time_in_force,
    }
    blob = json.dumps(material, sort_keys=True, separators=(",", ":")).encode()
    return "sha256:" + hashlib.sha256(blob).hexdigest()


def hash_of(proposal: TradeProposal) -> str:
    return compute_proposal_hash(
        proposal_id=proposal.id,
        intent_id=proposal.intent_id,
        user_id=proposal.user_id,
        mode=proposal.mode,
        symbol=proposal.symbol,
        side=proposal.side,
        order_type=proposal.order_type,
        quantity=proposal.quantity,
        limit_price=proposal.limit_price,
        time_in_force=proposal.time_in_force,
    )
