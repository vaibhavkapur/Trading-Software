"""Run configuration. Everything here is hashed into the run's ``config_hash``."""

from __future__ import annotations

from dataclasses import dataclass, field, fields
from decimal import Decimal, InvalidOperation
from typing import Any


class ConfigError(ValueError):
    """The configuration is invalid or inconsistent with the dataset."""


def _decimal(name: str, value: Any, *, low: Decimal | None = Decimal(0), high=None) -> Decimal:
    try:
        number = value if isinstance(value, Decimal) else Decimal(str(value))
    except InvalidOperation as exc:
        raise ConfigError(f"{name} must be a number, got {value!r}") from exc
    if not number.is_finite():
        raise ConfigError(f"{name} must be finite, got {value!r}")
    if low is not None and number < low:
        raise ConfigError(f"{name} must be >= {low}, got {number}")
    if high is not None and number > high:
        raise ConfigError(f"{name} must be <= {high}, got {number}")
    # One spelling per value, so that 1, 1.0 and 1e0 display and hash identically.
    return Decimal(format(number.normalize(), "f")) if number else Decimal(0)


def _non_negative(name: str, value: int | None, *, allow_none: bool = False) -> None:
    if value is None and allow_none:
        return
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise ConfigError(f"{name} must be a non-negative integer, got {value!r}")


@dataclass(frozen=True)
class ExecutionConfig:
    """Friction and exchange-simulation assumptions.

    fee_bps                 commission as basis points of traded notional, rounded up
    fee_per_unit            commission per unit traded, in price units
    slippage_bps            adverse price move applied to every liquidity-taking fill
    spread_bps              assumed full bid/ask spread for bar data; takers pay half.
                            Ignored for quote data, which carries its own spread.
    latency_ns              delay between a strategy sending a request and the exchange
                            receiving it, plus up to latency_jitter_ns of seeded jitter
    participation_rate      share of each bar's volume (or each quote's displayed size)
                            that the simulation may take. None means unlimited liquidity.
    touch_fill_probability  chance that a resting limit order fills when the market only
                            touches its price without trading through it
    max_quote_age_ns        an arriving order will not trade against a quote older than this
    """

    fee_bps: Decimal = Decimal(0)
    fee_per_unit: int = 0
    slippage_bps: Decimal = Decimal(0)
    spread_bps: Decimal = Decimal(0)
    latency_ns: int = 0
    latency_jitter_ns: int = 0
    participation_rate: Decimal | None = None
    touch_fill_probability: Decimal = Decimal(0)
    max_quote_age_ns: int | None = None

    def __post_init__(self) -> None:
        for name in ("fee_bps", "slippage_bps", "spread_bps"):
            object.__setattr__(self, name, _decimal(name, getattr(self, name)))
        object.__setattr__(
            self,
            "touch_fill_probability",
            _decimal("touch_fill_probability", self.touch_fill_probability, high=Decimal(1)),
        )
        if self.participation_rate is not None:
            rate = _decimal("participation_rate", self.participation_rate, high=Decimal(1))
            if rate == 0:
                raise ConfigError("participation_rate must be > 0; use None for unlimited")
            object.__setattr__(self, "participation_rate", rate)
        _non_negative("fee_per_unit", self.fee_per_unit)
        _non_negative("latency_ns", self.latency_ns)
        _non_negative("latency_jitter_ns", self.latency_jitter_ns)
        _non_negative("max_quote_age_ns", self.max_quote_age_ns, allow_none=True)


@dataclass(frozen=True)
class RiskLimits:
    """Pre-trade checks applied to every order intent before it leaves for the exchange."""

    max_order_qty: int | None = None
    max_order_notional: int | None = None
    max_position_qty: int | None = None
    max_open_orders: int | None = None
    max_data_age_ns: int | None = None
    allow_short: bool = False
    allow_leverage: bool = False

    def __post_init__(self) -> None:
        for name in (
            "max_order_qty",
            "max_order_notional",
            "max_position_qty",
            "max_open_orders",
            "max_data_age_ns",
        ):
            _non_negative(name, getattr(self, name), allow_none=True)


@dataclass(frozen=True)
class BacktestConfig:
    """One simulated account and its assumptions.

    initial_cash is in price units (see ``fixedpoint.to_units``). trade_from marks the end
    of a warm-up period during which the strategy observes data but may not trade.
    oos_start splits reported performance into in-sample and out-of-sample segments.
    """

    initial_cash: int = 1_000_000_0000
    price_decimals: int = 4
    seed: int = 0
    execution: ExecutionConfig = field(default_factory=ExecutionConfig)
    risk: RiskLimits = field(default_factory=RiskLimits)
    periods_per_year: int = 252
    risk_free_rate: Decimal = Decimal(0)
    trade_from: int | None = None
    oos_start: int | None = None

    def __post_init__(self) -> None:
        if isinstance(self.initial_cash, bool) or not isinstance(self.initial_cash, int):
            raise ConfigError("initial_cash must be an integer number of price units")
        if self.initial_cash <= 0:
            raise ConfigError("initial_cash must be positive")
        if not 0 <= self.price_decimals <= 9:
            raise ConfigError("price_decimals must be between 0 and 9")
        if isinstance(self.seed, bool) or not isinstance(self.seed, int):
            raise ConfigError("seed must be an integer")
        if self.periods_per_year <= 0:
            raise ConfigError("periods_per_year must be positive")
        object.__setattr__(
            self, "risk_free_rate", _decimal("risk_free_rate", self.risk_free_rate, low=None)
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "initial_cash": self.initial_cash,
            "price_decimals": self.price_decimals,
            "seed": self.seed,
            "execution": _section(self.execution),
            "risk": _section(self.risk),
            "periods_per_year": self.periods_per_year,
            "risk_free_rate": str(self.risk_free_rate),
            "trade_from": self.trade_from,
            "oos_start": self.oos_start,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> BacktestConfig:
        values = dict(data)
        values["execution"] = ExecutionConfig(**values.get("execution", {}))
        values["risk"] = RiskLimits(**values.get("risk", {}))
        return cls(**values)


def _section(section: Any) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for item in fields(section):
        value = getattr(section, item.name)
        out[item.name] = str(value) if isinstance(value, Decimal) else value
    return out
