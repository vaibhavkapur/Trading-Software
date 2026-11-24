"""API request and response Pydantic schemas."""

from __future__ import annotations

from datetime import datetime
from decimal import Decimal
from typing import Literal

from pydantic import BaseModel, Field

from options_infra.domain.models import (
    ExerciseStyle,
    GreekSnapshot,
    ModelWarning,
    MultiLegOrder,
    OptionContract,
    OptionPosition,
    OptionQuote,
    OptionRight,
    OrderLeg,
    PricingAssumptions,
    PricingModelType,
)
from options_infra.risk.aggregator import PortfolioGreeksSummary
from options_infra.risk.scenarios import ScenarioGridReport


class PriceRequest(BaseModel):
    right: OptionRight
    spot: float = Field(..., gt=0)
    strike: float = Field(..., gt=0)
    years: float = Field(..., ge=0)
    rate: float = 0.04
    dividend_yield: float = Field(default=0.0, ge=0)
    volatility: float = Field(..., ge=0)
    style: ExerciseStyle = ExerciseStyle.EUROPEAN
    model: PricingModelType = PricingModelType.BLACK_SCHOLES_MERTON
    binomial_steps: int = Field(default=200, ge=10, le=1000)


class PriceResponse(BaseModel):
    price: float
    delta: float
    gamma: float
    vega: float | None = None
    vega_per_1pct: float | None = None
    theta: float
    theta_per_day: float
    rho: float | None = None
    intrinsic_value: float
    time_value: float
    early_exercise_premium: float | None = None
    put_call_parity_residual: float | None = None
    assumptions: PricingAssumptions
    warnings: list[ModelWarning]


class IVRequest(BaseModel):
    market_price: float = Field(..., ge=0)
    right: OptionRight
    spot: float = Field(..., gt=0)
    strike: float = Field(..., gt=0)
    years: float = Field(..., gt=0)
    rate: float = 0.04
    dividend_yield: float = Field(default=0.0, ge=0)


class IVResponse(BaseModel):
    converged: bool
    implied_volatility: float | None
    iterations: int
    pricing_error: float | None
    status: str
    message: str


class ChainItem(BaseModel):
    contract: OptionContract
    quote: OptionQuote | None
    greeks: GreekSnapshot | None
    warnings: list[ModelWarning] = Field(default_factory=list)


class ChainResponse(BaseModel):
    symbol: str
    expiry: str
    underlying_px: Decimal | None
    execution_mode: Literal["PAPER_SIMULATED"] = "PAPER_SIMULATED"
    asof: datetime | None
    received_at: datetime | None
    rows: list[ChainItem]
    warnings: list[ModelWarning] = Field(default_factory=list)


class PortfolioGreeksRequest(BaseModel):
    positions: list[OptionPosition] = Field(..., min_length=1)
    valuation_time: datetime | None = None
    vol_overrides: dict[str, float] | None = None
    include_scenarios: bool = True
    spot_shocks_pct: list[float] | None = None
    vol_shocks_abs: list[float] | None = None
    days_forward: int = Field(default=0, ge=0, le=365)
    enforce_freshness: bool = False


class PortfolioGreeksResponse(BaseModel):
    summary: PortfolioGreeksSummary
    scenarios: ScenarioGridReport | None = None
    recalc_latency_ms: float


class VerticalSpreadPreviewRequest(BaseModel):
    long_contract_id: str
    short_contract_id: str
    quantity: int = Field(default=1, gt=0, le=1000)
    pricing_mode: Literal["MID", "NATURAL"] = "MID"
    valuation_time: datetime | None = None
    enforce_freshness: bool = False


class PaperOrderRequest(BaseModel):
    legs: list[OrderLeg] = Field(..., min_length=1)
    quantity: int = Field(default=1, gt=0, le=1000)
    strategy_type: str = "VERTICAL_SPREAD"
    account_id: str = "PAPER-DEFAULT"
    idempotency_key: str | None = None
    pricing_mode: Literal["MID", "NATURAL"] = "NATURAL"
    limit_net_debit_credit: Decimal | None = None
    auto_fill: bool = True
    asof: datetime | None = None


class PaperOrderResponse(BaseModel):
    order: MultiLegOrder
    deduplicated: bool
