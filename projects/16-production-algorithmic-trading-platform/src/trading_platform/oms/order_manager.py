"""Idempotent Order Management System with strict state transitions, race safety, and crash recovery."""

from __future__ import annotations

import uuid
from datetime import datetime
from decimal import Decimal
from typing import TYPE_CHECKING, Any

from trading_platform.domain.errors import (
    BrokerDisconnectedError,
    BrokerTimeoutError,
    DuplicateOrderConflictError,
    StateTransitionError,
)
from trading_platform.domain.events import (
    FillEvent,
    MarketTick,
    OrderIntent,
    OrderRecord,
    OrderState,
    OrderStateTransition,
    RiskCheckResult,
    Role,
    utc_now,
)
from trading_platform.ledger.ledger_service import LedgerService
from trading_platform.observability.telemetry import TelemetryRecorder
from trading_platform.risk.risk_engine import RiskEngine
from trading_platform.storage.sqlite_store import SQLiteStore

if TYPE_CHECKING:
    from trading_platform.broker.adapter import BrokerAdapter, CancelResponse, SubmitResponse

VALID_TRANSITIONS: dict[OrderState, set[OrderState]] = {
    OrderState.PENDING_NEW: {OrderState.RISK_REJECTED, OrderState.SUBMITTING},
    OrderState.SUBMITTING: {
        OrderState.ACKED,
        OrderState.PARTIALLY_FILLED,
        OrderState.FILLED,
        OrderState.REJECTED,
        OrderState.UNKNOWN_OUTCOME,
    },
    OrderState.ACKED: {
        OrderState.PARTIALLY_FILLED,
        OrderState.FILLED,
        OrderState.CANCEL_PENDING,
        OrderState.CANCELED,
        OrderState.REJECTED,
        OrderState.EXPIRED,
        OrderState.UNKNOWN_OUTCOME,
    },
    OrderState.PARTIALLY_FILLED: {
        OrderState.PARTIALLY_FILLED,
        OrderState.FILLED,
        OrderState.CANCEL_PENDING,
        OrderState.CANCELED,
        OrderState.EXPIRED,
        OrderState.UNKNOWN_OUTCOME,
    },
    OrderState.CANCEL_PENDING: {
        OrderState.CANCELED,
        OrderState.PARTIALLY_FILLED,
        OrderState.FILLED,
        OrderState.ACKED,
        OrderState.UNKNOWN_OUTCOME,
    },
    OrderState.UNKNOWN_OUTCOME: {
        OrderState.ACKED,
        OrderState.PARTIALLY_FILLED,
        OrderState.FILLED,
        OrderState.CANCELED,
        OrderState.REJECTED,
        OrderState.EXPIRED,
    },
    OrderState.RISK_REJECTED: set(),
    OrderState.FILLED: set(),
    OrderState.CANCELED: set(),
    OrderState.REJECTED: set(),
    OrderState.EXPIRED: set(),
}


class OrderManager:
    """Manages order lifecycle, idempotent submission, risk reservations, fills, and races."""

    def __init__(
        self,
        *,
        store: SQLiteStore,
        risk_engine: RiskEngine,
        ledger: LedgerService,
        broker: BrokerAdapter,
        telemetry: TelemetryRecorder,
    ) -> None:
        self.store = store
        self.risk_engine = risk_engine
        self.ledger = ledger
        self.broker = broker
        self.telemetry = telemetry

    def _transition(
        self,
        order: OrderRecord,
        to_state: OrderState,
        *,
        reason: str,
        actor: str = "OMS",
        updates: dict[str, Any] | None = None,
        metadata: dict[str, Any] | None = None,
    ) -> OrderRecord:
        allowed = VALID_TRANSITIONS.get(order.state, set())
        if to_state not in allowed:
            raise StateTransitionError(
                f"Invalid order transition for {order.client_order_id}: {order.state.value} -> {to_state.value}",
                details={
                    "client_order_id": order.client_order_id,
                    "from_state": order.state.value,
                    "to_state": to_state.value,
                },
            )
        now = utc_now()
        patch = dict(updates or {})
        patch["state"] = to_state
        patch["updated_at_utc"] = now
        updated = order.model_copy(update=patch)
        self.store.save_order(updated)
        self.store.save_transition(
            OrderStateTransition(
                transition_id=f"TRN-{uuid.uuid4().hex[:12].upper()}",
                client_order_id=order.client_order_id,
                from_state=order.state,
                to_state=to_state,
                reason=reason,
                actor=actor,
                occurred_at_utc=now,
                metadata=metadata or {},
            )
        )
        return updated

    def submit_order(
        self,
        intent: OrderIntent,
        *,
        mark_prices: dict[str, Decimal],
        reference_tick: MarketTick | None = None,
        now_utc: datetime | None = None,
    ) -> tuple[OrderRecord, RiskCheckResult | None, bool]:
        """
        Submits an order intent idempotently through deterministic risk checks to the broker.
        Returns (order_record, risk_check_result, was_idempotent_replay).
        """
        with self.telemetry.span(
            "oms.submit_order",
            attributes={
                "client_order_id": intent.client_order_id,
                "idempotency_key": intent.idempotency_key,
                "symbol": intent.symbol,
            },
            metric_name="oms_submit_latency_ms",
        ):
            payload_hash = intent.payload_fingerprint()
            existing = self.store.get_order_by_idempotency_key(intent.idempotency_key)
            if existing is not None:
                if existing.payload_hash != payload_hash:
                    self.telemetry.inc("orders_duplicate_conflict")
                    self.store.append_audit(
                        event_type="DUPLICATE_ORDER_CONFLICT",
                        actor=intent.strategy_id,
                        actor_role=Role.OPERATOR,
                        entity_type="ORDER",
                        entity_id=existing.client_order_id,
                        payload={
                            "idempotency_key": intent.idempotency_key,
                            "existing_hash": existing.payload_hash,
                            "incoming_hash": payload_hash,
                        },
                    )
                    raise DuplicateOrderConflictError(
                        f"Idempotency key {intent.idempotency_key} reused with conflicting order payload",
                        details={"idempotency_key": intent.idempotency_key},
                    )
                self.telemetry.inc("orders_idempotent_replays")
                return existing, None, True

            # Run deterministic pre-trade risk evaluation BEFORE any reservation or dispatch
            risk_result = self.risk_engine.evaluate(
                intent,
                mark_prices=mark_prices,
                reference_tick=reference_tick,
                now_utc=now_utc,
            )

            now = now_utc or utc_now()
            initial_order = OrderRecord(
                client_order_id=intent.client_order_id,
                idempotency_key=intent.idempotency_key,
                payload_hash=payload_hash,
                account_id=intent.account_id,
                strategy_id=intent.strategy_id,
                signal_id=intent.signal_id,
                symbol=intent.symbol,
                side=intent.side,
                order_type=intent.order_type,
                time_in_force=intent.time_in_force,
                quantity=intent.quantity,
                filled_quantity=Decimal("0"),
                remaining_quantity=intent.quantity,
                limit_price=intent.limit_price,
                reference_price=intent.reference_price,
                currency=intent.currency,
                state=OrderState.PENDING_NEW,
                execution_mode=self.risk_engine.policy.execution_mode,
                risk_check_id=risk_result.check_id,
                created_at_utc=now,
                updated_at_utc=now,
            )
            self.store.save_order(initial_order)
            self.store.save_transition(
                OrderStateTransition(
                    transition_id=f"TRN-{uuid.uuid4().hex[:12].upper()}",
                    client_order_id=initial_order.client_order_id,
                    from_state=None,
                    to_state=OrderState.PENDING_NEW,
                    reason="Order intent received by OMS",
                    actor=intent.strategy_id,
                    occurred_at_utc=now,
                )
            )

            if not risk_result.approved:
                reason = "; ".join(risk_result.blocking_reasons)
                rejected = self._transition(
                    initial_order,
                    OrderState.RISK_REJECTED,
                    reason=reason,
                    actor="RISK_ENGINE",
                    updates={"reject_reason": reason},
                )
                self.store.append_audit(
                    event_type="ORDER_RISK_REJECTED",
                    actor="RISK_ENGINE",
                    actor_role=Role.RISK_OFFICER,
                    entity_type="ORDER",
                    entity_id=rejected.client_order_id,
                    payload={"blocking_reasons": risk_result.blocking_reasons},
                )
                self.telemetry.inc("orders_risk_rejected")
                return rejected, risk_result, False

            # Reserve exposure BEFORE broker submission
            self.risk_engine.reserve(intent)
            submitting = self._transition(
                initial_order,
                OrderState.SUBMITTING,
                reason="Risk passed and exposure reserved; dispatching to broker",
                actor="OMS",
            )
            self.telemetry.inc("orders_submitted")

            try:
                response: SubmitResponse = self.broker.submit_order(
                    submitting, reference_tick=reference_tick
                )
            except (BrokerTimeoutError, BrokerDisconnectedError) as exc:
                # Uncertain outcome: MUST mark UNKNOWN_OUTCOME and RETAIN reserved exposure!
                unknown = self._transition(
                    submitting,
                    OrderState.UNKNOWN_OUTCOME,
                    reason=f"Ambiguous broker submit outcome: {exc.message}",
                    actor="OMS",
                    updates={"reject_reason": exc.message},
                    metadata={"error_code": exc.code},
                )
                self.risk_engine.retain_reservation_unknown(
                    unknown.client_order_id, exc.message
                )
                self.store.append_audit(
                    event_type="ORDER_UNKNOWN_OUTCOME",
                    actor="OMS",
                    actor_role=Role.OPERATOR,
                    entity_type="ORDER",
                    entity_id=unknown.client_order_id,
                    payload={
                        "error_code": exc.code,
                        "message": exc.message,
                        "reservation_retained": True,
                    },
                )
                self.telemetry.inc("orders_unknown_outcome")
                return unknown, risk_result, False

            order = self._apply_submit_response(submitting, response)
            return order, risk_result, False

    def _apply_submit_response(
        self, order: OrderRecord, response: SubmitResponse
    ) -> OrderRecord:
        if response.state == OrderState.REJECTED:
            rejected = self._transition(
                order,
                OrderState.REJECTED,
                reason=response.reject_reason or "Rejected by broker",
                actor="BROKER",
                updates={
                    "broker_order_id": response.broker_order_id,
                    "reject_reason": response.reject_reason,
                },
            )
            self.risk_engine.release_reservation(
                rejected.client_order_id, "Broker rejected order"
            )
            self.telemetry.inc("orders_broker_rejected")
            return rejected

        acked = self._transition(
            order,
            OrderState.ACKED,
            reason="Order acknowledged by broker",
            actor="BROKER",
            updates={"broker_order_id": response.broker_order_id},
        )
        self.telemetry.inc("orders_acked")

        current = acked
        for fill in response.fills:
            current = self.apply_fill(fill)
        return current

    def apply_fill(self, fill: FillEvent, *, actor: str = "BROKER") -> OrderRecord:
        """Idempotently applies a fill event, updating order state, ledger, and risk reservations."""
        order = self.store.get_order(fill.client_order_id)
        if order is None:
            raise StateTransitionError(
                f"Fill {fill.fill_id} references unknown client_order_id {fill.client_order_id}"
            )

        inserted = self.store.save_fill_if_absent(fill)
        if not inserted:
            self.telemetry.inc("fills_duplicate_ignored")
            return order

        # Recompute cumulative fills from durable store for exactness
        all_fills = self.store.list_fills(order.client_order_id)
        total_filled = sum((f.quantity for f in all_fills), Decimal("0"))
        total_notional = sum((f.quantity * f.price for f in all_fills), Decimal("0"))
        avg_px = (total_notional / total_filled).quantize(Decimal("0.0001"))
        remaining = max(Decimal("0"), order.quantity - total_filled)
        target_state = OrderState.FILLED if remaining == 0 else OrderState.PARTIALLY_FILLED

        # Cancel/fill race handling: if order was CANCEL_PENDING and fills complete,
        # transition cleanly to FILLED (or PARTIALLY_FILLED) instead of rejecting fill.
        updated = self._transition(
            order,
            target_state,
            reason=f"Fill {fill.fill_id} ({fill.quantity} @ {fill.price})",
            actor=actor,
            updates={
                "broker_order_id": fill.broker_order_id,
                "filled_quantity": total_filled,
                "remaining_quantity": remaining,
                "average_fill_price": avg_px,
            },
            metadata={"fill_id": fill.fill_id},
        )

        self.ledger.post_fill(fill)
        self.risk_engine.update_reservation_for_order(updated)
        self.store.append_audit(
            event_type="ORDER_FILL_APPLIED",
            actor=actor,
            actor_role=Role.OPERATOR,
            entity_type="ORDER",
            entity_id=updated.client_order_id,
            payload={
                "fill_id": fill.fill_id,
                "quantity": str(fill.quantity),
                "price": str(fill.price),
                "state": updated.state.value,
            },
        )
        self.telemetry.inc("fills_applied")
        if updated.state == OrderState.FILLED:
            self.telemetry.inc("orders_filled")
        return updated

    def cancel_order(
        self,
        client_order_id: str,
        *,
        actor: str = "OPERATOR",
        reason: str = "Operator cancel request",
    ) -> OrderRecord:
        """Cancels an open order while safely handling cancel/fill races and timeouts."""
        order = self.store.get_order(client_order_id)
        if order is None:
            raise StateTransitionError(f"Unknown order {client_order_id}")
        if order.state.is_terminal:
            return order

        pending = self._transition(
            order,
            OrderState.CANCEL_PENDING,
            reason=reason,
            actor=actor,
        )
        self.telemetry.inc("cancels_requested")

        try:
            cancel_resp: CancelResponse = self.broker.cancel_order(pending)
        except (BrokerTimeoutError, BrokerDisconnectedError) as exc:
            unknown = self._transition(
                pending,
                OrderState.UNKNOWN_OUTCOME,
                reason=f"Cancel outcome uncertain: {exc.message}",
                actor="OMS",
                metadata={"error_code": exc.code},
            )
            self.risk_engine.retain_reservation_unknown(
                unknown.client_order_id, f"Cancel uncertain: {exc.message}"
            )
            self.telemetry.inc("orders_unknown_outcome")
            return unknown

        current = pending
        # Apply any racing fills returned by broker BEFORE checking cancel status
        for fill in cancel_resp.racing_fills:
            current = self.apply_fill(fill, actor="BROKER_CANCEL_RACE")

        if current.state == OrderState.FILLED:
            # Fill won the race against cancel: record audit note, keep terminal FILLED state
            self.store.append_audit(
                event_type="CANCEL_FILL_RACE_RESOLVED_FILLED",
                actor="OMS",
                actor_role=Role.OPERATOR,
                entity_type="ORDER",
                entity_id=current.client_order_id,
                payload={"message": "Fill completed before cancel took effect; state remains FILLED"},
            )
            self.telemetry.inc("cancel_fill_races_resolved")
            return current

        if cancel_resp.canceled:
            canceled = self._transition(
                current,
                OrderState.CANCELED,
                reason=cancel_resp.reason or "Canceled by broker",
                actor="BROKER",
                updates={"remaining_quantity": Decimal("0")},
            )
            self.risk_engine.release_reservation(
                canceled.client_order_id, "Order canceled by broker"
            )
            self.store.append_audit(
                event_type="ORDER_CANCELED",
                actor=actor,
                actor_role=Role.OPERATOR,
                entity_type="ORDER",
                entity_id=canceled.client_order_id,
                payload={
                    "filled_quantity": str(canceled.filled_quantity),
                    "reason": cancel_resp.reason,
                },
            )
            self.telemetry.inc("orders_canceled")
            return canceled

        # Cancel was rejected by broker (e.g. still open)
        reverted_state = (
            OrderState.PARTIALLY_FILLED
            if current.filled_quantity > 0
            else OrderState.ACKED
        )
        reverted = self._transition(
            current,
            reverted_state,
            reason=cancel_resp.reason or "Cancel rejected by broker",
            actor="BROKER",
        )
        return reverted

    def resolve_order_from_broker(
        self,
        client_order_id: str,
        *,
        broker_order_id: str | None,
        broker_state: OrderState,
        broker_fills: list[FillEvent],
        reason: str = "Resolved via broker reconciliation",
    ) -> OrderRecord:
        """Resolves an UNKNOWN_OUTCOME or stale order using authoritative broker state."""
        order = self.store.get_order(client_order_id)
        if order is None:
            raise StateTransitionError(f"Cannot resolve unknown local order {client_order_id}")

        current = order
        for fill in broker_fills:
            current = self.apply_fill(fill, actor="RECONCILER")

        if current.state == broker_state or current.state.is_terminal:
            return current

        if broker_state in {OrderState.CANCELED, OrderState.REJECTED, OrderState.EXPIRED}:
            resolved = self._transition(
                current,
                broker_state,
                reason=reason,
                actor="RECONCILER",
                updates={
                    "broker_order_id": broker_order_id or current.broker_order_id,
                    "remaining_quantity": Decimal("0"),
                },
            )
            self.risk_engine.release_reservation(resolved.client_order_id, reason)
            return resolved

        if broker_state == OrderState.ACKED and current.state == OrderState.UNKNOWN_OUTCOME:
            resolved = self._transition(
                current,
                OrderState.ACKED,
                reason=reason,
                actor="RECONCILER",
                updates={"broker_order_id": broker_order_id or current.broker_order_id},
            )
            self.risk_engine.update_reservation_for_order(resolved)
            return resolved

        return current

    def cancel_all_open_orders(self, *, actor: str, reason: str) -> list[OrderRecord]:
        open_orders = self.store.list_orders(
            states={OrderState.ACKED, OrderState.PARTIALLY_FILLED}
        )
        results: list[OrderRecord] = []
        for o in open_orders:
            results.append(self.cancel_order(o.client_order_id, actor=actor, reason=reason))
        return results
