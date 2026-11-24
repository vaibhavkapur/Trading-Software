"""Core domain models for options contracts, quotes, Greeks, positions, and paper orders.

Design invariants:
- All timestamps are timezone-aware UTC; venue/event time (`asof`) is explicitly separated
  from local receive time (`received_at`).
- Monetary quantities (strikes, bid/ask, avg_price, debit/credit, max loss) use fixed-point
  `Decimal` arithmetic to avoid binary floating-point rounding drift in execution paths.
- Execution is strictly paper/simulated (`PAPER_SIMULATED`).
"""

from __future__ import annotations

from datetime import UTC, date, datetime
from decimal import Decimal
from enum import StrEnum
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator


def ensure_utc(dt: datetime) -> datetime:
    """Normalize a datetime to timezone-aware UTC; reject naive datetimes."""
    if dt.tzinfo is None or dt.utcoffset() is None:
        raise ValueError("Timestamps must be timezone-aware UTC datetimes")
    return dt.astimezone(UTC)


class OptionRight(StrEnum):
    CALL = "CALL"
    PUT = "PUT"


class ExerciseStyle(StrEnum):
    EUROPEAN = "EUROPEAN"
    AMERICAN = "AMERICAN"


class OrderSide(StrEnum):
    BUY = "BUY"
    SELL = "SELL"


class OrderStatus(StrEnum):
    PREVIEWED = "PREVIEWED"
    VALIDATED = "VALIDATED"
    SUBMITTED = "SUBMITTED"
    FILLED = "FILLED"
    REJECTED = "REJECTED"
    CANCELLED = "CANCELLED"


class PricingModelType(StrEnum):
    BLACK_SCHOLES_MERTON = "BLACK_SCHOLES_MERTON"
    BINOMIAL_CRR = "BINOMIAL_CRR"


class SpreadType(StrEnum):
    BULL_CALL_DEBIT = "BULL_CALL_DEBIT"
    BEAR_CALL_CREDIT = "BEAR_CALL_CREDIT"
    BULL_PUT_CREDIT = "BULL_PUT_CREDIT"
    BEAR_PUT_DEBIT = "BEAR_PUT_DEBIT"


class WarningSeverity(StrEnum):
    INFO = "INFO"
    WARNING = "WARNING"
    CRITICAL = "CRITICAL"


class ModelWarning(BaseModel):
    """Structured warning surfaced whenever model assumptions or operational risks apply."""

    model_config = ConfigDict(frozen=True)

    code: str
    severity: WarningSeverity
    message: str


class PricingAssumptions(BaseModel):
    """Explicit pricing assumptions attached to every valuation and payoff profile."""

    model_config = ConfigDict(frozen=True)

    model: PricingModelType = PricingModelType.BLACK_SCHOLES_MERTON
    model_version: str = "bsm-1.0.0"
    exercise_style_assumed: ExerciseStyle = ExerciseStyle.EUROPEAN
    day_count_convention: str = "ACT/365F"
    continuous_rate: float
    continuous_dividend_yield: float
    volatility_assumption: str = "Flat constant volatility per contract (no stochastic vol)"
    early_exercise_limitation: str = (
        "European BSM ignores early-exercise and dividend-capture assignment risk; "
        "short ITM options on American contracts may be assigned prior to expiration."
    )
    expiry_limitation: str = (
        "Expiration payoff ignores path-dependent margin calls, pin risk at strike, "
        "borrow fees, and transaction slippage unless explicitly simulated."
    )


class OptionContract(BaseModel):
    """Canonical option contract specification."""

    model_config = ConfigDict(frozen=True)

    id: str = Field(..., min_length=3, max_length=64)
    underlying: str = Field(..., min_length=1, max_length=16)
    expiry: date
    strike: Decimal = Field(..., gt=Decimal("0"))
    right: OptionRight
    multiplier: int = Field(default=100, gt=0, le=100_000)
    style: ExerciseStyle = ExerciseStyle.EUROPEAN
    currency: str = Field(default="USD", min_length=3, max_length=3)
    tick_size: Decimal = Field(default=Decimal("0.01"), gt=Decimal("0"))
    active: bool = True
    contract_version: str = "1.0"

    @field_validator("underlying")
    @classmethod
    def _upper_underlying(cls, value: str) -> str:
        return value.strip().upper()

    @field_validator("currency")
    @classmethod
    def _upper_currency(cls, value: str) -> str:
        return value.strip().upper()


class OptionQuote(BaseModel):
    """Point-in-time option quote with explicit venue event time vs local receive time."""

    model_config = ConfigDict(frozen=True)

    contract_id: str = Field(..., min_length=3, max_length=64)
    bid: Decimal = Field(..., ge=Decimal("0"))
    ask: Decimal = Field(..., ge=Decimal("0"))
    underlying_px: Decimal = Field(..., gt=Decimal("0"))
    asof: datetime = Field(..., description="UTC venue/event timestamp of the quote")
    received_at: datetime = Field(..., description="UTC local system receive timestamp")
    rate: Decimal = Field(default=Decimal("0.04"))
    dividend_yield: Decimal = Field(default=Decimal("0.00"), ge=Decimal("0"))
    source: str = Field(default="SIMULATED_FEED")
    sequence_id: int = Field(default=0, ge=0)

    @field_validator("asof", "received_at")
    @classmethod
    def _validate_utc(cls, value: datetime) -> datetime:
        return ensure_utc(value)

    @model_validator(mode="after")
    def _validate_spread(self) -> OptionQuote:
        if self.ask < self.bid:
            raise ValueError(f"Crossed quote: ask ({self.ask}) < bid ({self.bid})")
        return self

    @property
    def mid(self) -> Decimal:
        return (self.bid + self.ask) / Decimal("2")

    @property
    def spread(self) -> Decimal:
        return self.ask - self.bid

    @property
    def clock_skew_seconds(self) -> float:
        """Positive value indicates venue timestamp is ahead of local receive clock."""
        return (self.asof - self.received_at).total_seconds()


class GreekSnapshot(BaseModel):
    """Pricing and Greek sensitivities snapshot for a single option contract.

    Greek conventions:
    - `delta`: dV / dS (dimensionless, per 1 share of underlying)
    - `gamma`: d^2V / dS^2 (per 1 unit move in underlying)
    - `vega`: dV / d(sigma) (per 1.00 = 100% volatility move); `vega_per_1pct` = vega / 100
    - `theta`: -dV / dT (per calendar year); `theta_per_day` = theta / 365
    - `rho`: dV / dr (per 1.00 = 100% rate move)
    """

    model_config = ConfigDict(frozen=True)

    contract_id: str
    iv: float | None
    theoretical_price: float
    delta: float
    gamma: float
    vega: float
    vega_per_1pct: float
    theta: float
    theta_per_day: float
    rho: float
    model: PricingModelType = PricingModelType.BLACK_SCHOLES_MERTON
    model_version: str = "bsm-1.0.0"
    warnings: list[ModelWarning] = Field(default_factory=list)
    computed_at: datetime = Field(default_factory=lambda: datetime.now(UTC))

    @field_validator("computed_at")
    @classmethod
    def _validate_computed_at(cls, value: datetime) -> datetime:
        return ensure_utc(value)


class OptionPosition(BaseModel):
    """Signed option position in a paper portfolio."""

    model_config = ConfigDict(frozen=True)

    contract_id: str = Field(..., min_length=3, max_length=64)
    quantity: int = Field(..., description="Signed contracts: >0 long, <0 short")
    avg_price: Decimal = Field(..., ge=Decimal("0"))
    account_id: str = Field(default="PAPER-DEFAULT")

    @field_validator("quantity")
    @classmethod
    def _non_zero_qty(cls, value: int) -> int:
        if value == 0:
            raise ValueError("Position quantity must be non-zero")
        return value


class OrderLeg(BaseModel):
    """Single leg of a multi-leg paper option order."""

    model_config = ConfigDict(frozen=True)

    contract_id: str = Field(..., min_length=3, max_length=64)
    side: OrderSide
    ratio: int = Field(default=1, gt=0, le=100)
    limit_price: Decimal | None = Field(default=None, ge=Decimal("0"))


class MultiLegOrder(BaseModel):
    """Multi-leg paper order with fixed-point risk estimates and explicit lifecycle state."""

    model_config = ConfigDict(frozen=True)

    id: str
    idempotency_key: str | None = None
    account_id: str = Field(default="PAPER-DEFAULT")
    execution_mode: Literal["PAPER_SIMULATED"] = "PAPER_SIMULATED"
    strategy_type: str
    legs: list[OrderLeg] = Field(..., min_length=1)
    quantity: int = Field(default=1, gt=0, le=10_000)
    net_debit_credit_per_unit: Decimal = Field(
        ..., description="Per-spread option price (>0 debit, <0 credit)"
    )
    net_debit_credit: Decimal = Field(
        ..., description="Total currency debit (>0) or credit (<0) including quantity * multiplier"
    )
    max_loss_estimate: Decimal = Field(..., ge=Decimal("0"))
    max_gain_estimate: Decimal | None = None
    status: OrderStatus
    rejection_reason: str | None = None
    fill_price_per_unit: Decimal | None = None
    warnings: list[ModelWarning] = Field(default_factory=list)
    rule_version: str = "risk-rules-1.0.0"
    created_at: datetime
    updated_at: datetime

    @field_validator("created_at", "updated_at")
    @classmethod
    def _validate_timestamps(cls, value: datetime) -> datetime:
        return ensure_utc(value)
