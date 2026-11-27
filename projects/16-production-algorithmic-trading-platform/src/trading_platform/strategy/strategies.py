"""Deterministic, point-in-time algorithmic trading strategies."""

from __future__ import annotations

import math
from abc import ABC, abstractmethod
from collections import defaultdict, deque
from decimal import Decimal

from trading_platform.domain.config import StrategyConfig
from trading_platform.domain.events import MarketTick, Side, StrategySignal


class BaseStrategy(ABC):
    """Deterministic strategy interface consuming point-in-time MarketTick events."""

    def __init__(self, config: StrategyConfig) -> None:
        self.config = config
        self.enabled = config.enabled

    @property
    def strategy_id(self) -> str:
        return self.config.strategy_id

    @abstractmethod
    def on_tick(self, tick: MarketTick) -> StrategySignal | None:
        """Process a single tick and optionally emit a deterministic StrategySignal."""


class MomentumCrossoverStrategy(BaseStrategy):
    """Deterministic fast/slow simple moving average crossover strategy."""

    def __init__(self, config: StrategyConfig) -> None:
        super().__init__(config)
        self.fast_window = int(config.params.get("fast_window", 3))
        self.slow_window = int(config.params.get("slow_window", 6))
        self.order_quantity = Decimal(str(config.params.get("order_quantity", "25")))
        self.min_bps_gap = Decimal(str(config.params.get("min_bps_gap", "5")))
        self._prices: dict[str, deque[Decimal]] = defaultdict(
            lambda: deque(maxlen=self.slow_window)
        )
        self._last_regime: dict[str, str] = {}
        self._signal_seq = 0

    def on_tick(self, tick: MarketTick) -> StrategySignal | None:
        if not self.enabled or tick.symbol not in self.config.symbols:
            return None
        buf = self._prices[tick.symbol]
        buf.append(tick.mid)
        if len(buf) < self.slow_window:
            return None

        prices = list(buf)
        fast_avg = sum(prices[-self.fast_window :], Decimal("0")) / Decimal(self.fast_window)
        slow_avg = sum(prices, Decimal("0")) / Decimal(self.slow_window)
        if slow_avg <= 0:
            return None

        gap_bps = ((fast_avg - slow_avg) / slow_avg) * Decimal("10000")
        regime = self._last_regime.get(tick.symbol, "NEUTRAL")

        if gap_bps >= self.min_bps_gap and regime != "BULL":
            self._last_regime[tick.symbol] = "BULL"
            self._signal_seq += 1
            return StrategySignal(
                signal_id=f"SIG-{self.strategy_id}-{self._signal_seq:06d}",
                strategy_id=self.strategy_id,
                strategy_version=self.config.strategy_version,
                symbol=tick.symbol,
                side=Side.BUY,
                target_quantity=self.order_quantity,
                reference_price=tick.ask,
                confidence=Decimal("0.85"),
                reason=f"Fast SMA ({fast_avg:.2f}) crossed above Slow SMA ({slow_avg:.2f}) by {gap_bps:.1f}bps",
                source_tick_id=tick.tick_id,
                event_time_utc=tick.event_time_utc,
                available_time_utc=tick.available_time_utc,
            )

        if gap_bps <= -self.min_bps_gap and regime != "BEAR":
            self._last_regime[tick.symbol] = "BEAR"
            self._signal_seq += 1
            return StrategySignal(
                signal_id=f"SIG-{self.strategy_id}-{self._signal_seq:06d}",
                strategy_id=self.strategy_id,
                strategy_version=self.config.strategy_version,
                symbol=tick.symbol,
                side=Side.SELL,
                target_quantity=self.order_quantity,
                reference_price=tick.bid,
                confidence=Decimal("0.85"),
                reason=f"Fast SMA ({fast_avg:.2f}) crossed below Slow SMA ({slow_avg:.2f}) by {gap_bps:.1f}bps",
                source_tick_id=tick.tick_id,
                event_time_utc=tick.event_time_utc,
                available_time_utc=tick.available_time_utc,
            )

        return None


class MeanReversionStrategy(BaseStrategy):
    """Deterministic rolling z-score mean reversion strategy."""

    def __init__(self, config: StrategyConfig) -> None:
        super().__init__(config)
        self.window = int(config.params.get("window", 8))
        self.z_threshold = float(config.params.get("z_threshold", 1.5))
        self.order_quantity = Decimal(str(config.params.get("order_quantity", "20")))
        self._prices: dict[str, deque[Decimal]] = defaultdict(lambda: deque(maxlen=self.window))
        self._last_side: dict[str, Side | None] = {}
        self._signal_seq = 0

    def on_tick(self, tick: MarketTick) -> StrategySignal | None:
        if not self.enabled or tick.symbol not in self.config.symbols:
            return None
        buf = self._prices[tick.symbol]
        buf.append(tick.mid)
        if len(buf) < self.window:
            return None

        vals = [float(x) for x in buf]
        mean = sum(vals) / len(vals)
        variance = sum((x - mean) ** 2 for x in vals) / len(vals)
        std = math.sqrt(variance)
        if std <= 1e-9:
            return None

        z = (vals[-1] - mean) / std
        prev = self._last_side.get(tick.symbol)

        if z <= -self.z_threshold and prev != Side.BUY:
            self._last_side[tick.symbol] = Side.BUY
            self._signal_seq += 1
            return StrategySignal(
                signal_id=f"SIG-{self.strategy_id}-{self._signal_seq:06d}",
                strategy_id=self.strategy_id,
                strategy_version=self.config.strategy_version,
                symbol=tick.symbol,
                side=Side.BUY,
                target_quantity=self.order_quantity,
                reference_price=tick.ask,
                confidence=Decimal("0.75"),
                reason=f"Mean reversion oversold z={z:.2f} <= -{self.z_threshold}",
                source_tick_id=tick.tick_id,
                event_time_utc=tick.event_time_utc,
                available_time_utc=tick.available_time_utc,
            )

        if z >= self.z_threshold and prev != Side.SELL:
            self._last_side[tick.symbol] = Side.SELL
            self._signal_seq += 1
            return StrategySignal(
                signal_id=f"SIG-{self.strategy_id}-{self._signal_seq:06d}",
                strategy_id=self.strategy_id,
                strategy_version=self.config.strategy_version,
                symbol=tick.symbol,
                side=Side.SELL,
                target_quantity=self.order_quantity,
                reference_price=tick.bid,
                confidence=Decimal("0.75"),
                reason=f"Mean reversion overbought z={z:.2f} >= {self.z_threshold}",
                source_tick_id=tick.tick_id,
                event_time_utc=tick.event_time_utc,
                available_time_utc=tick.available_time_utc,
            )

        return None


def build_strategy(config: StrategyConfig) -> BaseStrategy:
    if config.strategy_type == "momentum_crossover":
        return MomentumCrossoverStrategy(config)
    if config.strategy_type == "mean_reversion":
        return MeanReversionStrategy(config)
    raise ValueError(f"Unsupported strategy_type: {config.strategy_type}")
