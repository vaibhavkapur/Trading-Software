"""Deterministic paper/sandbox broker adapter with explicit fault-injection controls."""

from __future__ import annotations

import threading
from dataclasses import dataclass, field
from decimal import Decimal

from trading_platform.broker.adapter import BrokerAdapter, CancelResponse, SubmitResponse
from trading_platform.domain.config import BrokerConfig
from trading_platform.domain.errors import BrokerDisconnectedError, BrokerTimeoutError
from trading_platform.domain.events import (
    BrokerAccountSnapshot,
    BrokerOrderSnapshot,
    FillEvent,
    MarketTick,
    OrderRecord,
    OrderState,
    Side,
    utc_now,
)

Q4 = Decimal("0.0001")


def _q(v: Decimal) -> Decimal:
    return v.quantize(Q4)


@dataclass
class _BrokerOrderBookEntry:
    broker_order_id: str
    client_order_id: str
    account_id: str
    strategy_id: str
    symbol: str
    side: Side
    quantity: Decimal
    reference_price: Decimal
    limit_price: Decimal | None
    currency: str
    filled_quantity: Decimal = Decimal("0")
    state: OrderState = OrderState.ACKED
    fills: list[FillEvent] = field(default_factory=list)


class PaperBrokerAdapter(BrokerAdapter):
    """
    Deterministic paper execution simulator with an independent broker-side accounting book
    and fault-injection hooks for scenario testing (timeouts, disconnects, cancel/fill races).
    """

    def __init__(
        self,
        *,
        config: BrokerConfig,
        account_id: str,
        initial_cash: Decimal,
        base_currency: str = "USD",
    ) -> None:
        self.config = config
        self.account_id = account_id
        self.base_currency = base_currency
        self._lock = threading.RLock()
        self._connected = True
        self._resting_mode = False
        self._order_seq = 0
        self._fill_seq = 0

        self._cash: Decimal = _q(initial_cash)
        self._positions: dict[str, Decimal] = {}
        self._orders: dict[str, _BrokerOrderBookEntry] = {}  # client_order_id -> entry
        self.submit_call_count = 0

        # Fault injection queues
        self._next_submit_timeout: tuple[bool, bool] | None = None  # (land_on_broker, fill_on_broker)
        self._next_submit_reject: str | None = None
        self._next_cancel_fill_race: Decimal | None = None
        self._next_cancel_timeout: bool = False

    # --- Fault Injection & Simulation Controls ---

    def set_connected(self, connected: bool) -> None:
        with self._lock:
            self._connected = connected

    def set_resting_mode(self, resting: bool) -> None:
        with self._lock:
            self._resting_mode = resting

    def inject_next_submit_timeout(
        self, *, land_on_broker: bool = True, fill_on_broker: bool = True
    ) -> None:
        """Simulates network timeout on submit; optionally lands and/or fills order on broker side."""
        with self._lock:
            self._next_submit_timeout = (land_on_broker, fill_on_broker)

    def inject_next_submit_reject(self, reason: str = "Simulated venue reject") -> None:
        with self._lock:
            self._next_submit_reject = reason

    def inject_next_cancel_fill_race(self, fill_ratio: Decimal = Decimal("1.0")) -> None:
        """Simulates an in-flight fill arriving just before a cancel request is processed."""
        with self._lock:
            self._next_cancel_fill_race = fill_ratio

    def inject_next_cancel_timeout(self) -> None:
        with self._lock:
            self._next_cancel_timeout = True

    def inject_broker_cash_drift(self, delta: Decimal) -> None:
        """Simulates an external broker fee/cash discrepancy for reconciliation testing."""
        with self._lock:
            self._cash = _q(self._cash + delta)

    def inject_orphan_broker_order(self, snapshot: BrokerOrderSnapshot) -> None:
        """Injects an order directly on broker side unknown to local OMS."""
        with self._lock:
            self._orders[snapshot.client_order_id] = _BrokerOrderBookEntry(
                broker_order_id=snapshot.broker_order_id,
                client_order_id=snapshot.client_order_id,
                account_id=self.account_id,
                strategy_id="EXTERNAL_MANUAL",
                symbol=snapshot.symbol,
                side=snapshot.side,
                quantity=snapshot.quantity,
                reference_price=snapshot.average_fill_price or Decimal("100.00"),
                limit_price=snapshot.average_fill_price,
                currency=self.base_currency,
                filled_quantity=snapshot.filled_quantity,
                state=snapshot.state,
                fills=list(snapshot.fills),
            )

    # --- BrokerAdapter Implementation ---

    def is_connected(self) -> bool:
        with self._lock:
            return self._connected

    def _compute_fill_price(
        self, order: OrderRecord, reference_tick: MarketTick | None
    ) -> Decimal:
        if reference_tick is not None:
            base = reference_tick.ask if order.side == Side.BUY else reference_tick.bid
        else:
            base = order.reference_price
        slip_factor = self.config.slippage_bps / Decimal("10000")
        if order.side == Side.BUY:
            px = base * (Decimal("1") + slip_factor)
            if order.limit_price is not None:
                px = min(px, order.limit_price)
        else:
            px = base * (Decimal("1") - slip_factor)
            if order.limit_price is not None:
                px = max(px, order.limit_price)
        return _q(px)

    def _compute_commission(self, qty: Decimal) -> Decimal:
        return _q(max(self.config.min_commission, qty * self.config.commission_per_share))

    def _book_broker_fill(
        self, entry: _BrokerOrderBookEntry, fill_qty: Decimal, fill_price: Decimal
    ) -> FillEvent:
        self._fill_seq += 1
        commission = self._compute_commission(fill_qty)
        fill = FillEvent(
            fill_id=f"FIL-{self._fill_seq:08d}",
            client_order_id=entry.client_order_id,
            broker_order_id=entry.broker_order_id,
            account_id=entry.account_id,
            strategy_id=entry.strategy_id,
            symbol=entry.symbol,
            side=entry.side,
            quantity=fill_qty,
            price=fill_price,
            commission=commission,
            currency=entry.currency,
            venue=self.config.venue_name,
            executed_at_utc=utc_now(),
        )
        entry.fills.append(fill)
        entry.filled_quantity += fill_qty
        entry.state = (
            OrderState.FILLED
            if entry.filled_quantity >= entry.quantity
            else OrderState.PARTIALLY_FILLED
        )

        # Update independent broker-side cash & position book
        gross = _q(fill_qty * fill_price)
        cur_pos = self._positions.get(entry.symbol, Decimal("0"))
        if entry.side == Side.BUY:
            self._positions[entry.symbol] = cur_pos + fill_qty
            self._cash = _q(self._cash - gross - commission)
        else:
            self._positions[entry.symbol] = cur_pos - fill_qty
            self._cash = _q(self._cash + gross - commission)
        return fill

    def submit_order(
        self, order: OrderRecord, *, reference_tick: MarketTick | None = None
    ) -> SubmitResponse:
        with self._lock:
            self.submit_call_count += 1
            if not self._connected:
                raise BrokerDisconnectedError(
                    "Paper broker session is disconnected",
                    details={"client_order_id": order.client_order_id},
                )

            # Broker-side idempotency guard on client_order_id
            if order.client_order_id in self._orders:
                existing = self._orders[order.client_order_id]
                return SubmitResponse(
                    broker_order_id=existing.broker_order_id,
                    state=existing.state,
                    fills=list(existing.fills),
                )

            if self._next_submit_reject is not None:
                reason = self._next_submit_reject
                self._next_submit_reject = None
                self._order_seq += 1
                broker_oid = f"BRK-{self._order_seq:08d}"
                return SubmitResponse(
                    broker_order_id=broker_oid,
                    state=OrderState.REJECTED,
                    reject_reason=reason,
                )

            timeout_cfg = self._next_submit_timeout
            self._next_submit_timeout = None

            self._order_seq += 1
            broker_oid = f"BRK-{self._order_seq:08d}"

            if timeout_cfg is not None:
                land_on_broker, fill_on_broker = timeout_cfg
                if land_on_broker:
                    entry = _BrokerOrderBookEntry(
                        broker_order_id=broker_oid,
                        client_order_id=order.client_order_id,
                        account_id=order.account_id,
                        strategy_id=order.strategy_id,
                        symbol=order.symbol,
                        side=order.side,
                        quantity=order.quantity,
                        reference_price=order.reference_price,
                        limit_price=order.limit_price,
                        currency=order.currency,
                    )
                    self._orders[order.client_order_id] = entry
                    if fill_on_broker:
                        fill_px = self._compute_fill_price(order, reference_tick)
                        self._book_broker_fill(entry, order.quantity, fill_px)
                raise BrokerTimeoutError(
                    f"Transport timeout waiting for broker ack on {order.client_order_id}",
                    details={"client_order_id": order.client_order_id},
                )

            entry = _BrokerOrderBookEntry(
                broker_order_id=broker_oid,
                client_order_id=order.client_order_id,
                account_id=order.account_id,
                strategy_id=order.strategy_id,
                symbol=order.symbol,
                side=order.side,
                quantity=order.quantity,
                reference_price=order.reference_price,
                limit_price=order.limit_price,
                currency=order.currency,
            )
            self._orders[order.client_order_id] = entry

            if self._resting_mode:
                return SubmitResponse(broker_order_id=broker_oid, state=OrderState.ACKED, fills=[])

            # Check limit crossability when reference_tick is provided
            if order.limit_price is not None and reference_tick is not None:
                if order.side == Side.BUY and order.limit_price < reference_tick.bid:
                    return SubmitResponse(
                        broker_order_id=broker_oid, state=OrderState.ACKED, fills=[]
                    )
                if order.side == Side.SELL and order.limit_price > reference_tick.ask:
                    return SubmitResponse(
                        broker_order_id=broker_oid, state=OrderState.ACKED, fills=[]
                    )

            fill_qty = (order.quantity * self.config.partial_fill_ratio).quantize(Decimal("1"))
            if fill_qty <= 0:
                fill_qty = order.quantity
            fill_qty = min(fill_qty, order.quantity)
            fill_px = self._compute_fill_price(order, reference_tick)
            fill = self._book_broker_fill(entry, fill_qty, fill_px)
            return SubmitResponse(
                broker_order_id=broker_oid,
                state=entry.state,
                fills=[fill],
            )

    def cancel_order(self, order: OrderRecord) -> CancelResponse:
        with self._lock:
            if not self._connected:
                raise BrokerDisconnectedError(
                    "Paper broker session is disconnected during cancel",
                    details={"client_order_id": order.client_order_id},
                )
            if self._next_cancel_timeout:
                self._next_cancel_timeout = False
                raise BrokerTimeoutError(
                    f"Timeout cancelling order {order.client_order_id}",
                    details={"client_order_id": order.client_order_id},
                )

            entry = self._orders.get(order.client_order_id)
            if entry is None:
                return CancelResponse(canceled=False, reason="Order not found at broker")

            if entry.state.is_terminal:
                return CancelResponse(
                    canceled=False,
                    reason=f"Order already terminal ({entry.state.value})",
                    racing_fills=list(entry.fills),
                )

            if self._next_cancel_fill_race is not None:
                ratio = self._next_cancel_fill_race
                self._next_cancel_fill_race = None
                remaining = entry.quantity - entry.filled_quantity
                race_qty = (remaining * ratio).quantize(Decimal("1"))
                race_qty = min(remaining, max(Decimal("1"), race_qty))
                fill_px = entry.limit_price or entry.reference_price
                fill = self._book_broker_fill(entry, race_qty, _q(fill_px))
                if entry.filled_quantity >= entry.quantity:
                    # Full fill beat the cancel
                    return CancelResponse(
                        canceled=False,
                        reason="Too late to cancel: filled before cancel processed",
                        racing_fills=[fill],
                    )
                # Partial fill beat the cancel, remaining portion canceled
                entry.state = OrderState.CANCELED
                return CancelResponse(
                    canceled=True,
                    reason="Partially filled before cancel; remainder canceled",
                    racing_fills=[fill],
                )

            entry.state = OrderState.CANCELED
            return CancelResponse(canceled=True, reason="Canceled at paper venue")

    def _entry_to_snapshot(self, entry: _BrokerOrderBookEntry) -> BrokerOrderSnapshot:
        avg_px: Decimal | None = None
        if entry.fills:
            tot_qty = sum((f.quantity for f in entry.fills), Decimal("0"))
            tot_val = sum((f.quantity * f.price for f in entry.fills), Decimal("0"))
            avg_px = _q(tot_val / tot_qty)
        return BrokerOrderSnapshot(
            broker_order_id=entry.broker_order_id,
            client_order_id=entry.client_order_id,
            symbol=entry.symbol,
            side=entry.side,
            quantity=entry.quantity,
            filled_quantity=entry.filled_quantity,
            average_fill_price=avg_px,
            state=entry.state,
            fills=list(entry.fills),
            updated_at_utc=utc_now(),
        )

    def fetch_order(self, client_order_id: str) -> BrokerOrderSnapshot | None:
        with self._lock:
            if not self._connected:
                raise BrokerDisconnectedError("Broker disconnected on fetch_order")
            entry = self._orders.get(client_order_id)
            return self._entry_to_snapshot(entry) if entry else None

    def fetch_account_snapshot(self) -> BrokerAccountSnapshot:
        with self._lock:
            if not self._connected:
                raise BrokerDisconnectedError("Broker disconnected on fetch_account_snapshot")
            non_zero_positions = {k: v for k, v in self._positions.items() if v != 0}
            return BrokerAccountSnapshot(
                account_id=self.account_id,
                currency=self.base_currency,
                cash_balance=self._cash,
                positions=non_zero_positions,
                orders=[self._entry_to_snapshot(e) for e in self._orders.values()],
                as_of_utc=utc_now(),
            )
