"""Deterministic strategy scheduler converting validated ticks into idempotent order intents."""

from __future__ import annotations

from decimal import Decimal

from trading_platform.domain.events import (
    MarketTick,
    OrderIntent,
    OrderType,
    Side,
    StrategySignal,
    TimeInForce,
)
from trading_platform.strategy.strategies import BaseStrategy


class StrategyScheduler:
    """Runs registered strategies in deterministic registration order over validated ticks."""

    def __init__(self, *, account_id: str, base_currency: str = "USD") -> None:
        self.account_id = account_id
        self.base_currency = base_currency
        self._strategies: list[BaseStrategy] = []

    def register(self, strategy: BaseStrategy) -> None:
        self._strategies.append(strategy)

    def list_strategies(self) -> list[dict[str, object]]:
        return [
            {
                "strategy_id": s.strategy_id,
                "strategy_type": s.config.strategy_type,
                "version": s.config.strategy_version,
                "symbols": list(s.config.symbols),
                "enabled": s.enabled,
            }
            for s in self._strategies
        ]

    def set_enabled(self, strategy_id: str, enabled: bool) -> bool:
        for s in self._strategies:
            if s.strategy_id == strategy_id:
                s.enabled = enabled
                return True
        return False

    def on_tick(self, tick: MarketTick) -> list[tuple[StrategySignal, OrderIntent]]:
        produced: list[tuple[StrategySignal, OrderIntent]] = []
        for strategy in self._strategies:
            if not strategy.enabled:
                continue
            signal = strategy.on_tick(tick)
            if signal is None:
                continue
            # Limit price padded by 10 bps to stay well inside price collar while crossing top-of-book
            pad = Decimal("0.0010")
            limit_px = (
                (tick.ask * (Decimal("1") + pad)).quantize(Decimal("0.01"))
                if signal.side == Side.BUY
                else (tick.bid * (Decimal("1") - pad)).quantize(Decimal("0.01"))
            )
            idem_key = f"IDEM:{strategy.strategy_id}:{tick.symbol}:{tick.tick_id}:{signal.side.value}"
            client_oid = f"ORD-{strategy.strategy_id}-{tick.tick_id}-{signal.side.value}"
            intent = OrderIntent(
                idempotency_key=idem_key,
                client_order_id=client_oid,
                account_id=self.account_id,
                strategy_id=strategy.strategy_id,
                signal_id=signal.signal_id,
                symbol=signal.symbol,
                side=signal.side,
                order_type=OrderType.LIMIT,
                time_in_force=TimeInForce.DAY,
                quantity=signal.target_quantity,
                limit_price=limit_px,
                reference_price=signal.reference_price,
                currency=self.base_currency,
                created_at_utc=tick.available_time_utc,
            )
            produced.append((signal, intent))
        return produced
