"""Order domain model and the order manager's state machine."""

from __future__ import annotations

from dataclasses import dataclass
from enum import StrEnum

from .fixedpoint import div_round_half_even


class Side(StrEnum):
    BUY = "buy"
    SELL = "sell"

    @property
    def sign(self) -> int:
        return 1 if self is Side.BUY else -1


class OrderType(StrEnum):
    MARKET = "market"
    LIMIT = "limit"


class TimeInForce(StrEnum):
    GTC = "gtc"  # works until filled or canceled
    IOC = "ioc"  # fills what it can at its first matching opportunity, then cancels


class OrderStatus(StrEnum):
    PENDING = "pending"  # accepted by risk, in flight to the exchange
    WORKING = "working"
    PARTIALLY_FILLED = "partially_filled"
    FILLED = "filled"
    CANCELED = "canceled"
    REJECTED = "rejected"


class Liquidity(StrEnum):
    TAKER = "taker"
    MAKER = "maker"


_TERMINAL = frozenset({OrderStatus.FILLED, OrderStatus.CANCELED, OrderStatus.REJECTED})
_TRANSITIONS: dict[OrderStatus, frozenset[OrderStatus]] = {
    OrderStatus.PENDING: frozenset({OrderStatus.WORKING, OrderStatus.CANCELED}),
    OrderStatus.WORKING: frozenset(
        {OrderStatus.PARTIALLY_FILLED, OrderStatus.FILLED, OrderStatus.CANCELED}
    ),
    OrderStatus.PARTIALLY_FILLED: frozenset(
        {OrderStatus.PARTIALLY_FILLED, OrderStatus.FILLED, OrderStatus.CANCELED}
    ),
    OrderStatus.FILLED: frozenset(),
    OrderStatus.CANCELED: frozenset(),
    OrderStatus.REJECTED: frozenset(),
}


class OrderStateError(RuntimeError):
    """An order was asked to make a transition its lifecycle does not allow."""


@dataclass(frozen=True, slots=True)
class SimOrder:
    """An order intent as accepted by the order manager. Never mutated.

    decision_px is the last price the strategy could see when it decided; execution
    shortfall is measured against it.
    """

    id: str
    symbol: str
    side: Side
    type: OrderType
    qty: int
    limit_px: int | None
    submitted_at: int
    tif: TimeInForce = TimeInForce.GTC
    tag: str | None = None
    decision_px: int | None = None


@dataclass(frozen=True, slots=True)
class SimFill:
    id: str
    order_id: str
    symbol: str
    side: Side
    qty: int
    px: int
    fee: int
    ts: int
    liquidity: Liquidity


@dataclass(frozen=True, slots=True)
class OrderCancel:
    order_id: str
    reason: str
    ts: int


@dataclass(frozen=True, slots=True)
class OrderView:
    """A read-only snapshot of an order's state, safe to hand to strategies and reports."""

    id: str
    symbol: str
    side: Side
    type: OrderType
    qty: int
    limit_px: int | None
    tif: TimeInForce
    tag: str | None
    submitted_at: int
    decision_px: int | None
    status: OrderStatus
    filled_qty: int
    filled_notional: int
    fees: int
    arrived_at: int | None
    closed_at: int | None
    reason: str | None

    @property
    def remaining(self) -> int:
        return self.qty - self.filled_qty

    @property
    def is_open(self) -> bool:
        return self.status not in _TERMINAL

    @property
    def avg_fill_px(self) -> int | None:
        if self.filled_qty == 0:
            return None
        return div_round_half_even(self.filled_notional, self.filled_qty)


class _OrderState:
    __slots__ = (
        "arrived_at",
        "closed_at",
        "fees",
        "filled_notional",
        "filled_qty",
        "order",
        "reason",
        "status",
    )

    def __init__(self, order: SimOrder) -> None:
        self.order = order
        self.status = OrderStatus.PENDING
        self.filled_qty = 0
        self.filled_notional = 0
        self.fees = 0
        self.arrived_at: int | None = None
        self.closed_at: int | None = None
        self.reason: str | None = None

    def view(self) -> OrderView:
        order = self.order
        return OrderView(
            id=order.id,
            symbol=order.symbol,
            side=order.side,
            type=order.type,
            qty=order.qty,
            limit_px=order.limit_px,
            tif=order.tif,
            tag=order.tag,
            submitted_at=order.submitted_at,
            decision_px=order.decision_px,
            status=self.status,
            filled_qty=self.filled_qty,
            filled_notional=self.filled_notional,
            fees=self.fees,
            arrived_at=self.arrived_at,
            closed_at=self.closed_at,
            reason=self.reason,
        )


class OrderManager:
    """Owns order identity and lifecycle. The only writer of order state."""

    def __init__(self) -> None:
        self._orders: dict[str, _OrderState] = {}
        self._open: dict[str, _OrderState] = {}
        self._next = 0

    def next_id(self) -> str:
        self._next += 1
        return f"O{self._next:07d}"

    def register(self, order: SimOrder, reject_reason: str | None = None) -> OrderView:
        if order.id in self._orders:
            raise OrderStateError(f"duplicate order id {order.id}")
        state = _OrderState(order)
        self._orders[order.id] = state
        if reject_reason is None:
            self._open[order.id] = state
        else:
            state.status = OrderStatus.REJECTED
            state.reason = reject_reason
            state.closed_at = order.submitted_at
        return state.view()

    def on_arrival(self, order_id: str, ts: int) -> None:
        state = self._state(order_id)
        self._move(state, OrderStatus.WORKING)
        state.arrived_at = ts

    def on_fill(self, fill: SimFill) -> OrderView:
        state = self._state(fill.order_id)
        if fill.qty <= 0 or state.filled_qty + fill.qty > state.order.qty:
            raise OrderStateError(
                f"fill of {fill.qty} would overfill {fill.order_id} "
                f"({state.filled_qty}/{state.order.qty})"
            )
        done = state.filled_qty + fill.qty == state.order.qty
        self._move(state, OrderStatus.FILLED if done else OrderStatus.PARTIALLY_FILLED)
        state.filled_qty += fill.qty
        state.filled_notional += fill.qty * fill.px
        state.fees += fill.fee
        if done:
            self._close(state, fill.ts)
        return state.view()

    def on_cancel(self, cancel: OrderCancel) -> OrderView:
        state = self._state(cancel.order_id)
        self._move(state, OrderStatus.CANCELED)
        state.reason = cancel.reason
        self._close(state, cancel.ts)
        return state.view()

    def view(self, order_id: str) -> OrderView:
        return self._state(order_id).view()

    def is_open(self, order_id: str) -> bool:
        return order_id in self._open

    def knows(self, order_id: str) -> bool:
        return order_id in self._orders

    def open_orders(self, symbol: str | None = None) -> tuple[OrderView, ...]:
        return tuple(
            state.view()
            for state in self._open.values()
            if symbol is None or state.order.symbol == symbol
        )

    def open_count(self) -> int:
        return len(self._open)

    def all_orders(self) -> tuple[OrderView, ...]:
        return tuple(state.view() for state in self._orders.values())

    def _state(self, order_id: str) -> _OrderState:
        try:
            return self._orders[order_id]
        except KeyError:
            raise OrderStateError(f"unknown order id {order_id!r}") from None

    def _move(self, state: _OrderState, target: OrderStatus) -> None:
        if target not in _TRANSITIONS[state.status]:
            raise OrderStateError(
                f"order {state.order.id} cannot go from {state.status} to {target}"
            )
        state.status = target

    def _close(self, state: _OrderState, ts: int) -> None:
        state.closed_at = ts
        del self._open[state.order.id]
