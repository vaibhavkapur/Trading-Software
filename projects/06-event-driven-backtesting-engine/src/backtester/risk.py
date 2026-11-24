"""Pre-trade risk gate.

A pure function of the order intent and the account state visible at decision time.
It runs before latency is applied, so a rejected order never reaches the exchange.
"""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass

from .config import RiskLimits
from .fees import FeeModel
from .orders import OrderType, OrderView, Side, SimOrder

INVALID_QTY = "INVALID_QTY"
INVALID_LIMIT_PRICE = "INVALID_LIMIT_PRICE"
WARMUP = "WARMUP"
RUN_ENDED = "RUN_ENDED"
NO_MARKET_DATA = "NO_MARKET_DATA"
STALE_MARKET_DATA = "STALE_MARKET_DATA"
MAX_OPEN_ORDERS = "MAX_OPEN_ORDERS"
MAX_ORDER_QTY = "MAX_ORDER_QTY"
MAX_ORDER_NOTIONAL = "MAX_ORDER_NOTIONAL"
MAX_POSITION_QTY = "MAX_POSITION_QTY"
SHORT_NOT_ALLOWED = "SHORT_NOT_ALLOWED"
INSUFFICIENT_CASH = "INSUFFICIENT_CASH"


@dataclass(frozen=True, slots=True)
class AccountState:
    """What the gate may look at: the account as the strategy could know it right now."""

    now: int
    cash: int
    position_qty: int
    mark: int | None
    last_data_ts: int | None
    open_orders: tuple[OrderView, ...]


def validate(order: SimOrder) -> str | None:
    """Structural checks that do not depend on account state."""
    if isinstance(order.qty, bool) or not isinstance(order.qty, int) or order.qty <= 0:
        return INVALID_QTY
    if order.type is OrderType.LIMIT:
        px = order.limit_px
        if isinstance(px, bool) or not isinstance(px, int) or px <= 0:
            return INVALID_LIMIT_PRICE
    elif order.limit_px is not None:
        return INVALID_LIMIT_PRICE
    return None


class RiskGate:
    def __init__(self, limits: RiskLimits, fees: FeeModel) -> None:
        self._limits = limits
        self._fees = fees

    def check(self, order: SimOrder, account: AccountState) -> str | None:
        """Return a reject reason code, or None if the order may be sent."""
        limits = self._limits
        if account.mark is None or account.last_data_ts is None:
            return NO_MARKET_DATA
        if (
            limits.max_data_age_ns is not None
            and account.now - account.last_data_ts > limits.max_data_age_ns
        ):
            return STALE_MARKET_DATA
        if (
            limits.max_open_orders is not None
            and len(account.open_orders) >= limits.max_open_orders
        ):
            return MAX_OPEN_ORDERS
        if limits.max_order_qty is not None and order.qty > limits.max_order_qty:
            return MAX_ORDER_QTY

        ref_px = order.limit_px if order.type is OrderType.LIMIT else account.mark
        notional = order.qty * ref_px
        if limits.max_order_notional is not None and notional > limits.max_order_notional:
            return MAX_ORDER_NOTIONAL

        # Assume every open order on the same side fills: the worst case for this check.
        same_symbol = [o for o in account.open_orders if o.symbol == order.symbol]
        pending = sum(o.remaining for o in same_symbol if o.side is order.side)
        worst_position = account.position_qty + order.side.sign * (pending + order.qty)
        if limits.max_position_qty is not None and abs(worst_position) > limits.max_position_qty:
            return MAX_POSITION_QTY
        if order.side is Side.SELL and worst_position < 0 and not limits.allow_short:
            return SHORT_NOT_ALLOWED

        if order.side is Side.BUY and not limits.allow_leverage:
            cost = notional + self._fees.fee(notional, order.qty)
            if cost > account.cash - self._reserved(account.open_orders):
                return INSUFFICIENT_CASH
        return None

    def _reserved(self, open_orders: Iterable[OrderView]) -> int:
        """Cash already committed to open buy orders, at their reference price."""
        total = 0
        for other in open_orders:
            if other.side is not Side.BUY:
                continue
            px = other.limit_px if other.type is OrderType.LIMIT else other.decision_px
            notional = other.remaining * (px or 0)
            total += notional + self._fees.fee(notional, other.remaining)
        return total
