"""Order management: state machine, authorised idempotent dispatch, fill ingestion.

The OMS holds the only write-capable broker handle. `submit` is the single code path
that can reach `broker.submit_order`, and it re-verifies the approval and re-runs the
risk gate against the *stored proposal* immediately before dispatch.

Dispatch is idempotent on `client_order_id`. If a dispatch attempt fails at the
connection level the outcome is unknown, so the order stays in SUBMITTING and the next
submit first asks the broker whether it already has the order before sending again.
"""

from __future__ import annotations

from collections.abc import Callable
from decimal import ROUND_HALF_UP, Decimal

from .approvals import ApprovalInvalid, ApprovalService
from .audit import AuditLog
from .broker.base import BrokerAPI, BrokerOrderRequest, BrokerOrderStatus, BrokerUnavailable
from .clock import Clock
from .domain import (
    TERMINAL_ORDER_STATES,
    Approval,
    Order,
    OrderState,
    ProposalStatus,
    RiskDecision,
    TradeProposal,
    check_transition,
)
from .metrics import Metrics
from .store import Store


class BrokerUnavailableError(Exception):
    """Dispatch could not be confirmed. Safe to retry; nothing is assumed filled."""

    def __init__(self, order: Order) -> None:
        super().__init__("broker unavailable; order outcome unconfirmed")
        self.order = order


_BROKER_STATE = {
    "accepted": OrderState.ACCEPTED,
    "partially_filled": OrderState.PARTIALLY_FILLED,
    "filled": OrderState.FILLED,
    "canceled": OrderState.CANCELED,
    "rejected": OrderState.REJECTED,
}

PreTradeCheck = Callable[[TradeProposal, str], RiskDecision]


class OrderManager:
    def __init__(
        self,
        store: Store,
        broker: BrokerAPI,
        approvals: ApprovalService,
        pre_trade_check: PreTradeCheck,
        audit: AuditLog,
        metrics: Metrics,
        clock: Clock,
        max_submit_attempts: int = 3,
    ) -> None:
        self._store = store
        self._db = store.db
        self._broker = broker
        self._approvals = approvals
        self._pre_trade_check = pre_trade_check
        self._audit = audit
        self._metrics = metrics
        self._clock = clock
        self.max_submit_attempts = max_submit_attempts

    # -- creation -----------------------------------------------------------
    def create_order(self, proposal: TradeProposal, approval: Approval, order_id: str) -> Order:
        now = self._clock.now()
        order = Order(
            client_order_id=order_id,
            proposal_id=proposal.id,
            approval_id=approval.id,
            correlation_id=proposal.correlation_id,
            user_id=proposal.user_id,
            symbol=proposal.symbol,
            side=proposal.side,
            order_type=proposal.order_type,
            quantity=proposal.quantity,
            limit_price=proposal.limit_price,
            state=OrderState.CREATED,
            created_at=now,
            updated_at=now,
        )
        self._db.insert("orders", order)
        self._event(order, "order.created", {"approval_id": approval.id})
        return order

    # -- submit -------------------------------------------------------------
    def submit(self, order_id: str) -> Order:
        with self._db.lock:
            order = self._require(order_id)

            if order.state in TERMINAL_ORDER_STATES or order.state in (
                OrderState.ACCEPTED,
                OrderState.PARTIALLY_FILLED,
            ):
                # Already dispatched (or dead): a repeat submit is a no-op.
                self._metrics.order_submissions.labels("duplicate").inc()
                self._event(order, "order.submit_duplicate", {"state": order.state.value})
                return order

            if order.state is OrderState.SUBMITTING:
                # A previous attempt may or may not have reached the broker. Ask first.
                try:
                    known = self._broker.get_order(order.client_order_id)
                except BrokerUnavailable:
                    self._metrics.order_submissions.labels("broker_unavailable").inc()
                    self._event(order, "order.submit_unconfirmed", {"phase": "reconcile"})
                    raise BrokerUnavailableError(order) from None
                if known is not None:
                    self._metrics.reconciliation_mismatches.labels("unacked_order").inc()
                    self._event(order, "order.reconciled", {"broker_state": known.state})
                    self._metrics.order_submissions.labels("recovered").inc()
                    return self._apply(order, known)

            proposal = self._store.proposal(order.proposal_id)
            reason = self._authorisation_failure(order, proposal)
            if reason is None:
                decision = self._pre_trade_check(proposal, "pre_submit")
                if not decision.passed:
                    reason = "RISK:" + ",".join(v.code for v in decision.violations)
            if reason is not None:
                self._metrics.order_submissions.labels("rejected").inc()
                return self._reject(order, reason)

            # Authorised. Dispatch exactly the approved parameters, read from the proposal.
            request = BrokerOrderRequest(
                client_order_id=order.client_order_id,
                user_id=proposal.user_id,
                symbol=proposal.symbol,
                side=proposal.side,
                order_type=proposal.order_type,
                quantity=proposal.quantity,
                limit_price=proposal.limit_price,
            )
            order = self._transition(order, OrderState.SUBMITTING)
            status: BrokerOrderStatus | None = None
            for attempt in range(1, self.max_submit_attempts + 1):
                order = self._update(order, submit_attempts=order.submit_attempts + 1)
                try:
                    status = self._broker.submit_order(request)
                    break
                except BrokerUnavailable as exc:
                    self._event(
                        order, "order.dispatch_failed", {"attempt": attempt, "error": str(exc)}
                    )
                    if attempt < self.max_submit_attempts:
                        self._metrics.submit_retries.inc()
            if status is None:
                self._metrics.order_submissions.labels("broker_unavailable").inc()
                self._event(order, "order.submit_unconfirmed", {"phase": "dispatch"})
                raise BrokerUnavailableError(order)

            self._metrics.order_submissions.labels(
                "rejected" if status.state == "rejected" else "accepted"
            ).inc()
            self._event(
                order,
                "order.dispatched",
                {"broker_order_id": status.broker_order_id, "broker_state": status.state},
            )
            return self._apply(order, status)

    def _authorisation_failure(self, order: Order, proposal: TradeProposal | None) -> str | None:
        approval = self._store.approval(order.approval_id)
        if proposal is None or approval is None:
            return "APPROVAL_MISSING"
        try:
            self._approvals.verify(approval, proposal, self._clock.now())
        except ApprovalInvalid as exc:
            return exc.code
        if proposal.status is not ProposalStatus.APPROVED:
            return "PROPOSAL_NOT_APPROVED"
        same = (
            order.proposal_id == proposal.id
            and order.user_id == proposal.user_id
            and order.symbol == proposal.symbol
            and order.side == proposal.side
            and order.order_type == proposal.order_type
            and order.quantity == proposal.quantity
            and order.limit_price == proposal.limit_price
        )
        return None if same else "ORDER_CHANGED"

    # -- refresh / cancel ---------------------------------------------------
    def refresh(self, order_id: str) -> Order:
        """Pull broker state and fills for a working order. Best effort."""
        with self._db.lock:
            order = self._require(order_id)
            if order.state not in (
                OrderState.SUBMITTING,
                OrderState.ACCEPTED,
                OrderState.PARTIALLY_FILLED,
            ):
                return order
            try:
                status = self._broker.get_order(order.client_order_id)
            except BrokerUnavailable:
                return order
            if status is None:
                return order
            if order.state is OrderState.SUBMITTING:
                self._metrics.reconciliation_mismatches.labels("unacked_order").inc()
                self._event(order, "order.reconciled", {"broker_state": status.state})
            return self._apply(order, status)

    def cancel(self, order_id: str) -> Order:
        with self._db.lock:
            order = self._require(order_id)
            if order.state in TERMINAL_ORDER_STATES:
                return order
            if order.state is OrderState.CREATED:
                order = self._transition(order, OrderState.CANCELED)
                self._finish_proposal(order)
                return order
            try:
                status = self._broker.cancel_order(order.client_order_id)
            except BrokerUnavailable:
                self._event(order, "order.cancel_unconfirmed", {})
                raise BrokerUnavailableError(order) from None
            if status is None:
                # Broker never received it, and now it never will.
                order = self._transition(order, OrderState.CANCELED)
                self._finish_proposal(order)
                return order
            return self._apply(order, status)

    # -- internals ----------------------------------------------------------
    def _require(self, order_id: str) -> Order:
        order = self._store.order(order_id)
        if order is None:
            raise KeyError(order_id)
        return order

    def _event(self, order: Order, event_type: str, payload: dict) -> None:
        self._audit.append(
            event_type,
            correlation_id=order.correlation_id,
            actor="oms",
            payload={"order_id": order.client_order_id, **payload},
        )

    def _update(self, order: Order, **changes) -> Order:
        changes["updated_at"] = self._clock.now()
        updated = order.model_copy(update=changes)
        dumped = updated.model_dump(mode="json")
        self._db.update(
            "orders", "client_order_id", order.client_order_id, {k: dumped[k] for k in changes}
        )
        return updated

    def _transition(self, order: Order, new: OrderState, **changes) -> Order:
        check_transition(order.state, new)
        if new is order.state and not changes:
            return order
        updated = self._update(order, state=new, **changes)
        if new is not order.state:
            self._event(
                updated, "order.state_changed", {"from": order.state.value, "to": new.value}
            )
        return updated

    def _reject(self, order: Order, reason: str) -> Order:
        with self._db.transaction():
            order = self._transition(order, OrderState.REJECTED, reject_reason=reason)
            self._event(order, "order.rejected", {"reason": reason})
            self._finish_proposal(order)
        return order

    def _finish_proposal(self, order: Order) -> None:
        if order.state is OrderState.FILLED or order.filled_qty > 0:
            status = ProposalStatus.EXECUTED
        else:
            status = ProposalStatus.FAILED
        proposal = self._store.proposal(order.proposal_id)
        if proposal is not None and proposal.status is ProposalStatus.APPROVED:
            self._store.set_proposal_status(order.proposal_id, status, self._clock.now())

    def _apply(self, order: Order, status: BrokerOrderStatus) -> Order:
        """Fold a broker status report into local state. Idempotent."""
        with self._db.transaction():
            now = self._clock.now()
            for fill in status.fills:
                inserted = self._db.insert(
                    "fills",
                    fill.model_copy(
                        update={"client_order_id": order.client_order_id, "received_at": now}
                    ),
                    or_ignore=True,
                )
                if inserted:
                    self._event(
                        order,
                        "order.fill",
                        {
                            "fill_id": fill.fill_id,
                            "quantity": fill.quantity,
                            "price": str(fill.price),
                            "venue_time": fill.venue_time.isoformat(),
                        },
                    )
            fills = self._store.fills(order.client_order_id)
            filled_qty = sum(f.quantity for f in fills)
            avg = None
            if filled_qty:
                avg = (sum(f.price * f.quantity for f in fills) / filled_qty).quantize(
                    Decimal("0.0001"), rounding=ROUND_HALF_UP
                )
            if filled_qty != status.filled_qty:
                self._metrics.reconciliation_mismatches.labels("filled_qty").inc()
                self._event(
                    order,
                    "order.reconciliation_mismatch",
                    {"local_filled_qty": filled_qty, "broker_filled_qty": status.filled_qty},
                )
            new_state = _BROKER_STATE[status.state]
            order = self._transition(
                order,
                new_state,
                broker_order_id=status.broker_order_id,
                filled_qty=filled_qty,
                avg_fill_price=avg,
                reject_reason=(
                    f"BROKER:{status.reject_reason}"
                    if status.reject_reason
                    else order.reject_reason
                ),
            )
            if order.state in TERMINAL_ORDER_STATES:
                self._finish_proposal(order)
            return order
