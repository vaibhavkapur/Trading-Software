"""Canonical versioned event contracts for market data, orders, risk, ledger, and reconciliation."""

from __future__ import annotations

import hashlib
import json
from datetime import UTC, datetime
from decimal import Decimal
from enum import StrEnum
from typing import Any

from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator

SCHEMA_VERSION = "1.0.0"


def utc_now() -> datetime:
    return datetime.now(UTC)


def ensure_utc(dt: datetime) -> datetime:
    if dt.tzinfo is None:
        raise ValueError("Timestamp must be timezone-aware UTC")
    return dt.astimezone(UTC)


class ExecutionMode(StrEnum):
    DISABLED = "DISABLED"
    PAPER = "PAPER"
    SHADOW = "SHADOW"
    LIVE = "LIVE"


class Side(StrEnum):
    BUY = "BUY"
    SELL = "SELL"


class OrderType(StrEnum):
    MARKET = "MARKET"
    LIMIT = "LIMIT"


class TimeInForce(StrEnum):
    DAY = "DAY"
    IOC = "IOC"
    GTC = "GTC"


class OrderState(StrEnum):
    PENDING_NEW = "PENDING_NEW"
    RISK_REJECTED = "RISK_REJECTED"
    SUBMITTING = "SUBMITTING"
    ACKED = "ACKED"
    PARTIALLY_FILLED = "PARTIALLY_FILLED"
    FILLED = "FILLED"
    CANCEL_PENDING = "CANCEL_PENDING"
    CANCELED = "CANCELED"
    REJECTED = "REJECTED"
    UNKNOWN_OUTCOME = "UNKNOWN_OUTCOME"
    EXPIRED = "EXPIRED"

    @property
    def is_terminal(self) -> bool:
        return self in {
            OrderState.RISK_REJECTED,
            OrderState.FILLED,
            OrderState.CANCELED,
            OrderState.REJECTED,
            OrderState.EXPIRED,
        }

    @property
    def holds_reservation(self) -> bool:
        return self in {
            OrderState.PENDING_NEW,
            OrderState.SUBMITTING,
            OrderState.ACKED,
            OrderState.PARTIALLY_FILLED,
            OrderState.CANCEL_PENDING,
            OrderState.UNKNOWN_OUTCOME,
        }


class KillSwitchScope(StrEnum):
    GLOBAL = "GLOBAL"
    STRATEGY = "STRATEGY"
    SYMBOL = "SYMBOL"


class Role(StrEnum):
    VIEWER = "VIEWER"
    AUDITOR = "AUDITOR"
    OPERATOR = "OPERATOR"
    RISK_OFFICER = "RISK_OFFICER"
    COMPLIANCE = "COMPLIANCE"
    CTO = "CTO"


class FeedEntitlement(BaseModel):
    model_config = ConfigDict(frozen=True)

    schema_version: str = SCHEMA_VERSION
    feed_id: str
    provider: str
    license_type: str  # e.g., SYNTHETIC_INTERNAL, PERMITTED_DELAYED_SANDBOX
    asset_class: str = "US_EQUITIES"
    allowed_modes: list[ExecutionMode] = Field(
        default_factory=lambda: [ExecutionMode.PAPER, ExecutionMode.SHADOW]
    )
    symbols: list[str]
    provenance_uri: str
    valid_until_utc: datetime

    @field_validator("valid_until_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class MarketTick(BaseModel):
    model_config = ConfigDict(frozen=True)

    schema_version: str = SCHEMA_VERSION
    tick_id: str
    feed_id: str
    symbol: str
    currency: str = "USD"
    bid: Decimal
    ask: Decimal
    last: Decimal
    volume: Decimal = Decimal("0")
    sequence_no: int = Field(ge=0)
    event_time_utc: datetime
    available_time_utc: datetime

    @field_validator("event_time_utc", "available_time_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)

    @model_validator(mode="after")
    def _validate_prices(self) -> MarketTick:
        if self.bid <= 0 or self.ask <= 0 or self.last <= 0:
            raise ValueError("Tick prices must be strictly positive")
        if self.bid > self.ask:
            raise ValueError(f"Crossed book tick: bid {self.bid} > ask {self.ask}")
        if self.volume < 0:
            raise ValueError("Volume cannot be negative")
        return self

    @property
    def mid(self) -> Decimal:
        return (self.bid + self.ask) / Decimal("2")


class StrategySignal(BaseModel):
    model_config = ConfigDict(frozen=True)

    schema_version: str = SCHEMA_VERSION
    signal_id: str
    strategy_id: str
    strategy_version: str
    symbol: str
    side: Side
    target_quantity: Decimal
    reference_price: Decimal
    confidence: Decimal = Decimal("1.0")
    reason: str
    source_tick_id: str
    event_time_utc: datetime
    available_time_utc: datetime

    @field_validator("event_time_utc", "available_time_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)

    @model_validator(mode="after")
    def _validate_qty(self) -> StrategySignal:
        if self.target_quantity <= 0:
            raise ValueError("Signal target_quantity must be positive")
        if self.reference_price <= 0:
            raise ValueError("Signal reference_price must be positive")
        return self


class OrderIntent(BaseModel):
    model_config = ConfigDict(frozen=True)

    schema_version: str = SCHEMA_VERSION
    idempotency_key: str = Field(min_length=4, max_length=128)
    client_order_id: str = Field(min_length=4, max_length=128)
    account_id: str
    strategy_id: str
    signal_id: str | None = None
    symbol: str
    side: Side
    order_type: OrderType = OrderType.LIMIT
    time_in_force: TimeInForce = TimeInForce.DAY
    quantity: Decimal
    limit_price: Decimal | None = None
    reference_price: Decimal
    currency: str = "USD"
    created_at_utc: datetime = Field(default_factory=utc_now)

    @field_validator("created_at_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)

    @model_validator(mode="after")
    def _validate_intent(self) -> OrderIntent:
        if self.quantity <= 0:
            raise ValueError("Order quantity must be strictly positive")
        if self.reference_price <= 0:
            raise ValueError("Order reference_price must be strictly positive")
        if self.order_type == OrderType.LIMIT and (
            self.limit_price is None or self.limit_price <= 0
        ):
            raise ValueError("LIMIT order requires strictly positive limit_price")
        if self.order_type == OrderType.MARKET and self.limit_price is not None:
            raise ValueError("MARKET order must not set limit_price")
        return self

    @property
    def effective_price(self) -> Decimal:
        return self.limit_price if self.limit_price is not None else self.reference_price

    @property
    def notional(self) -> Decimal:
        return (self.quantity * self.effective_price).quantize(Decimal("0.0001"))

    def payload_fingerprint(self) -> str:
        canonical = {
            "account_id": self.account_id,
            "strategy_id": self.strategy_id,
            "symbol": self.symbol,
            "side": self.side.value,
            "order_type": self.order_type.value,
            "time_in_force": self.time_in_force.value,
            "quantity": str(self.quantity),
            "limit_price": str(self.limit_price) if self.limit_price is not None else None,
            "currency": self.currency,
        }
        return hashlib.sha256(
            json.dumps(canonical, sort_keys=True, separators=(",", ":")).encode("utf-8")
        ).hexdigest()


class RiskRuleOutcome(BaseModel):
    model_config = ConfigDict(frozen=True)

    rule_id: str
    passed: bool
    reason: str
    observed_value: str | None = None
    limit_value: str | None = None


class RiskCheckResult(BaseModel):
    model_config = ConfigDict(frozen=True)

    schema_version: str = SCHEMA_VERSION
    check_id: str
    client_order_id: str
    approved: bool
    policy_version: str
    checked_at_utc: datetime = Field(default_factory=utc_now)
    outcomes: list[RiskRuleOutcome]
    blocking_reasons: list[str] = Field(default_factory=list)

    @field_validator("checked_at_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class RiskReservation(BaseModel):
    model_config = ConfigDict(frozen=True)

    schema_version: str = SCHEMA_VERSION
    reservation_id: str
    client_order_id: str
    account_id: str
    strategy_id: str
    symbol: str
    side: Side
    reserved_quantity: Decimal
    reserved_notional: Decimal
    currency: str = "USD"
    active: bool = True
    retained_reason: str | None = None
    created_at_utc: datetime = Field(default_factory=utc_now)
    updated_at_utc: datetime = Field(default_factory=utc_now)

    @field_validator("created_at_utc", "updated_at_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class KillSwitchState(BaseModel):
    model_config = ConfigDict(frozen=True)

    schema_version: str = SCHEMA_VERSION
    scope: KillSwitchScope
    target: str  # "ALL", strategy_id, or symbol
    engaged: bool
    reason: str
    actor: str
    actor_role: Role
    requires_dual_release: bool = False
    updated_at_utc: datetime = Field(default_factory=utc_now)

    @field_validator("updated_at_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class OrderStateTransition(BaseModel):
    model_config = ConfigDict(frozen=True)

    schema_version: str = SCHEMA_VERSION
    transition_id: str
    client_order_id: str
    from_state: OrderState | None
    to_state: OrderState
    reason: str
    actor: str
    occurred_at_utc: datetime = Field(default_factory=utc_now)
    metadata: dict[str, Any] = Field(default_factory=dict)

    @field_validator("occurred_at_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class OrderRecord(BaseModel):
    model_config = ConfigDict(frozen=True)

    schema_version: str = SCHEMA_VERSION
    client_order_id: str
    idempotency_key: str
    payload_hash: str
    broker_order_id: str | None = None
    account_id: str
    strategy_id: str
    signal_id: str | None = None
    symbol: str
    side: Side
    order_type: OrderType
    time_in_force: TimeInForce
    quantity: Decimal
    filled_quantity: Decimal = Decimal("0")
    remaining_quantity: Decimal
    limit_price: Decimal | None = None
    reference_price: Decimal
    average_fill_price: Decimal | None = None
    currency: str = "USD"
    state: OrderState = OrderState.PENDING_NEW
    execution_mode: ExecutionMode = ExecutionMode.PAPER
    reject_reason: str | None = None
    risk_check_id: str | None = None
    created_at_utc: datetime = Field(default_factory=utc_now)
    updated_at_utc: datetime = Field(default_factory=utc_now)

    @field_validator("created_at_utc", "updated_at_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class FillEvent(BaseModel):
    model_config = ConfigDict(frozen=True)

    schema_version: str = SCHEMA_VERSION
    fill_id: str
    client_order_id: str
    broker_order_id: str
    account_id: str
    strategy_id: str
    symbol: str
    side: Side
    quantity: Decimal
    price: Decimal
    commission: Decimal = Decimal("0")
    currency: str = "USD"
    venue: str = "PAPER_SANDBOX"
    executed_at_utc: datetime = Field(default_factory=utc_now)

    @field_validator("executed_at_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)

    @model_validator(mode="after")
    def _validate_fill(self) -> FillEvent:
        if self.quantity <= 0:
            raise ValueError("Fill quantity must be strictly positive")
        if self.price <= 0:
            raise ValueError("Fill price must be strictly positive")
        if self.commission < 0:
            raise ValueError("Commission cannot be negative")
        return self

    @property
    def gross_notional(self) -> Decimal:
        return (self.quantity * self.price).quantize(Decimal("0.0001"))


class JournalLine(BaseModel):
    model_config = ConfigDict(frozen=True)

    account_code: str
    currency: str = "USD"
    debit: Decimal = Decimal("0")
    credit: Decimal = Decimal("0")
    symbol: str | None = None
    memo: str = ""

    @model_validator(mode="after")
    def _validate_line(self) -> JournalLine:
        if self.debit < 0 or self.credit < 0:
            raise ValueError("Debit and credit amounts cannot be negative")
        if self.debit > 0 and self.credit > 0:
            raise ValueError("A journal line cannot have both debit and credit > 0")
        if self.debit == 0 and self.credit == 0:
            raise ValueError("A journal line must have either debit or credit > 0")
        return self


class LedgerEntry(BaseModel):
    model_config = ConfigDict(frozen=True)

    schema_version: str = SCHEMA_VERSION
    entry_id: str
    idempotency_key: str
    account_id: str
    entry_type: str  # CASH_DEPOSIT, FILL, COMMISSION, REALIZED_PNL
    reference_id: str
    description: str
    posted_at_utc: datetime = Field(default_factory=utc_now)
    lines: list[JournalLine]

    @field_validator("posted_at_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)

    @model_validator(mode="after")
    def _validate_balanced(self) -> LedgerEntry:
        if len(self.lines) < 2:
            raise ValueError("Double-entry ledger requires at least 2 journal lines")
        by_currency: dict[str, tuple[Decimal, Decimal]] = {}
        for line in self.lines:
            d, c = by_currency.get(line.currency, (Decimal("0"), Decimal("0")))
            by_currency[line.currency] = (d + line.debit, c + line.credit)
        for ccy, (debits, credits) in by_currency.items():
            if debits != credits:
                raise ValueError(
                    f"Unbalanced ledger entry {self.entry_id} for {ccy}: "
                    f"debits={debits} != credits={credits}"
                )
        return self


class PositionRecord(BaseModel):
    model_config = ConfigDict(frozen=True)

    account_id: str
    symbol: str
    currency: str = "USD"
    quantity: Decimal = Decimal("0")
    average_cost: Decimal = Decimal("0")
    realized_pnl: Decimal = Decimal("0")
    total_commissions: Decimal = Decimal("0")
    updated_at_utc: datetime = Field(default_factory=utc_now)

    @field_validator("updated_at_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class BrokerOrderSnapshot(BaseModel):
    model_config = ConfigDict(frozen=True)

    broker_order_id: str
    client_order_id: str
    symbol: str
    side: Side
    quantity: Decimal
    filled_quantity: Decimal
    average_fill_price: Decimal | None = None
    state: OrderState
    fills: list[FillEvent] = Field(default_factory=list)
    updated_at_utc: datetime = Field(default_factory=utc_now)

    @field_validator("updated_at_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class BrokerAccountSnapshot(BaseModel):
    model_config = ConfigDict(frozen=True)

    account_id: str
    currency: str = "USD"
    cash_balance: Decimal
    positions: dict[str, Decimal]  # symbol -> signed quantity
    orders: list[BrokerOrderSnapshot]
    as_of_utc: datetime = Field(default_factory=utc_now)

    @field_validator("as_of_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class ReconciliationBreakType(StrEnum):
    UNKNOWN_ORDER_RESOLVED = "UNKNOWN_ORDER_RESOLVED"
    MISSING_BROKER_ORDER = "MISSING_BROKER_ORDER"
    ORPHAN_BROKER_ORDER = "ORPHAN_BROKER_ORDER"
    ORDER_STATE_MISMATCH = "ORDER_STATE_MISMATCH"
    POSITION_QTY_BREAK = "POSITION_QTY_BREAK"
    CASH_BALANCE_BREAK = "CASH_BALANCE_BREAK"
    RESERVED_EXPOSURE_DRIFT = "RESERVED_EXPOSURE_DRIFT"


class ReconciliationBreakSeverity(StrEnum):
    INFO = "INFO"
    WARNING = "WARNING"
    CRITICAL = "CRITICAL"


class ReconciliationBreak(BaseModel):
    model_config = ConfigDict(frozen=True)

    break_id: str
    break_type: ReconciliationBreakType
    severity: ReconciliationBreakSeverity
    entity_id: str
    local_value: str
    broker_value: str
    delta: str
    resolution: str


class ReconciliationReport(BaseModel):
    model_config = ConfigDict(frozen=True)

    schema_version: str = SCHEMA_VERSION
    reconciliation_id: str
    account_id: str
    run_at_utc: datetime = Field(default_factory=utc_now)
    matched: bool
    unknown_orders_resolved: int = 0
    local_cash: Decimal
    broker_cash: Decimal
    cash_delta: Decimal
    local_positions: dict[str, Decimal]
    broker_positions: dict[str, Decimal]
    breaks: list[ReconciliationBreak] = Field(default_factory=list)
    kill_switch_triggered: bool = False

    @field_validator("run_at_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class AuditEvent(BaseModel):
    model_config = ConfigDict(frozen=True)

    schema_version: str = SCHEMA_VERSION
    sequence_no: int = Field(ge=1)
    event_id: str
    event_type: str
    actor: str
    actor_role: Role
    entity_type: str
    entity_id: str
    payload: dict[str, Any] = Field(default_factory=dict)
    occurred_at_utc: datetime = Field(default_factory=utc_now)
    prev_hash: str
    event_hash: str

    @field_validator("occurred_at_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class ShadowDecisionRecord(BaseModel):
    model_config = ConfigDict(frozen=True)

    schema_version: str = SCHEMA_VERSION
    shadow_id: str
    tick_id: str
    strategy_id: str
    symbol: str
    side: Side
    quantity: Decimal
    reference_price: Decimal
    risk_approved: bool
    blocking_reasons: list[str] = Field(default_factory=list)
    dispatched_to_broker: bool = False
    recorded_at_utc: datetime = Field(default_factory=utc_now)

    @field_validator("recorded_at_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class LiveApprovalRecord(BaseModel):
    model_config = ConfigDict(frozen=True)

    approver_id: str
    role: Role
    approval_ref: str
    signed_at_utc: datetime
    expires_at_utc: datetime
    attestation: str

    @field_validator("signed_at_utc", "expires_at_utc")
    @classmethod
    def _val_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class LiveRolloutBundle(BaseModel):
    model_config = ConfigDict(frozen=True)

    bundle_id: str
    venue_readiness_verified: bool = False
    regulatory_readiness_verified: bool = False
    independent_risk_review_ref: str | None = None
    security_review_ref: str | None = None
    soak_report_ref: str | None = None
    approvals: list[LiveApprovalRecord] = Field(default_factory=list)
