"""Constrained Signal DSL — allowlisted, parameterized signals without code execution.

The agent MUST express trading hypotheses using only these declarative signal
definitions. Arbitrary Python code, eval expressions, or negative (future) lags
are rejected at both schema and validator layers.
"""

from __future__ import annotations

from enum import StrEnum
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, field_validator


class SignalType(StrEnum):
    """Allowlisted deterministic signal families."""

    ABNORMAL_VOLUME_REVERSAL = "abnormal_volume_reversal"
    ABNORMAL_VOLUME_MOMENTUM = "abnormal_volume_momentum"
    CROSS_SECTIONAL_MOMENTUM = "cross_sectional_momentum"
    MEAN_REVERSION_ZSCORE = "mean_reversion_zscore"
    VOLATILITY_BREAKOUT = "volatility_breakout"
    MOVING_AVERAGE_CROSSOVER = "moving_average_crossover"


class RebalanceFrequency(StrEnum):
    """Allowlisted rebalance schedules."""

    DAILY = "daily"
    WEEKLY = "weekly"


class DirectionMode(StrEnum):
    """Position directionality."""

    LONG_ONLY = "long_only"
    LONG_SHORT = "long_short"


FORBIDDEN_CODE_TOKENS = (
    "__import__",
    "eval(",
    "exec(",
    "lambda ",
    "open(",
    "os.",
    "subprocess",
    "import ",
    "shift(-",
    ".shift(-",
)


class SignalDefinition(BaseModel):
    """Declarative, strictly bounded signal specification."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    signal_type: SignalType
    lookback_days: int = Field(
        default=20,
        ge=2,
        le=252,
        description="Trailing window in trading days (>= 2, <= 252).",
    )
    short_window: int = Field(
        default=5,
        ge=1,
        le=60,
        description="Short trailing window for crossover/volume ratio signals.",
    )
    zscore_threshold: float = Field(
        default=1.5,
        ge=0.25,
        le=5.0,
        description="Trigger threshold in standard deviations or ratio units.",
    )
    lag_bars: int = Field(
        default=1,
        ge=1,
        le=5,
        description=(
            "Execution lag in bars (>= 1). Signals computed at bar t close "
            "execute no earlier than bar t + lag_bars open."
        ),
    )
    direction: DirectionMode = DirectionMode.LONG_SHORT
    rebalance: RebalanceFrequency = RebalanceFrequency.DAILY
    max_positions: int = Field(default=5, ge=1, le=50)
    custom_expression: str | None = Field(
        default=None,
        description="Explicitly forbidden in MVP; rejected by validator if populated.",
    )
    feature_shift: int = Field(
        default=0,
        description=(
            "Additional past lag on features (must be >= 0; negative values indicate "
            "lookahead leakage and are rejected by validator)."
        ),
    )

    @field_validator("short_window")
    @classmethod
    def check_short_window(cls, v: int) -> int:
        if v < 1:
            raise ValueError("short_window must be >= 1")
        return v


class TransactionCostAssumptions(BaseModel):
    """Explicit transaction cost assumptions in basis points (fixed-point friendly)."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    commission_bps: float = Field(default=1.5, ge=0.0, le=100.0)
    half_spread_bps: float = Field(default=2.0, ge=0.0, le=100.0)
    impact_bps_per_pct_adv: float = Field(
        default=10.0,
        ge=0.0,
        le=250.0,
        description="Market impact in bps per 1% of trailing ADV traded.",
    )
    borrow_fee_annual_bps: float = Field(
        default=50.0,
        ge=0.0,
        le=1000.0,
        description="Annualized short-borrow fee in bps applied to short notional.",
    )


class WalkForwardConfig(BaseModel):
    """Optional walk-forward evaluation configuration."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    enabled: bool = False
    num_folds: int = Field(default=3, ge=2, le=10)
    expanding: bool = True
    step_days: int = Field(default=42, ge=10, le=252)


ExecutionPriceMode = Literal["next_open"]
