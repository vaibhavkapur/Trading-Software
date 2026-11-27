"""Reconciles OMS orders, reserved exposure, double-entry ledger cash, and positions against broker state."""

from __future__ import annotations

import uuid
from decimal import Decimal

from trading_platform.broker.adapter import BrokerAdapter
from trading_platform.domain.events import (
    BrokerAccountSnapshot,
    KillSwitchScope,
    OrderState,
    ReconciliationBreak,
    ReconciliationBreakSeverity,
    ReconciliationBreakType,
    ReconciliationReport,
    Role,
    utc_now,
)
from trading_platform.ledger.ledger_service import LedgerService
from trading_platform.observability.telemetry import TelemetryRecorder
from trading_platform.oms.order_manager import OrderManager
from trading_platform.risk.risk_engine import RiskEngine
from trading_platform.storage.sqlite_store import SQLiteStore

CASH_TOLERANCE = Decimal("0.01")


class BrokerReconciler:
    """Resolves unknown order outcomes and detects order, position, cash, and reservation breaks."""

    def __init__(
        self,
        *,
        store: SQLiteStore,
        oms: OrderManager,
        ledger: LedgerService,
        risk_engine: RiskEngine,
        broker: BrokerAdapter,
        telemetry: TelemetryRecorder,
    ) -> None:
        self.store = store
        self.oms = oms
        self.ledger = ledger
        self.risk_engine = risk_engine
        self.broker = broker
        self.telemetry = telemetry

    def reconcile(
        self,
        *,
        broker_snapshot: BrokerAccountSnapshot | None = None,
        resolve_unknown_orders: bool = True,
        actor: str = "RECONCILER",
    ) -> ReconciliationReport:
        with self.telemetry.span(
            "reconciler.reconcile",
            attributes={"account_id": self.ledger.account_id},
            metric_name="reconciliation_latency_ms",
        ):
            snap = broker_snapshot or self.broker.fetch_account_snapshot()
            breaks: list[ReconciliationBreak] = []
            unknown_resolved = 0

            broker_orders_by_cid = {o.client_order_id: o for o in snap.orders}
            local_orders = self.store.list_orders()
            local_orders_by_cid = {o.client_order_id: o for o in local_orders}

            # Phase 1: Resolve UNKNOWN_OUTCOME and SUBMITTING orders against broker state
            if resolve_unknown_orders:
                for order in local_orders:
                    if order.state not in {OrderState.UNKNOWN_OUTCOME, OrderState.SUBMITTING}:
                        continue
                    b_ord = broker_orders_by_cid.get(order.client_order_id)
                    if b_ord is not None:
                        self.oms.resolve_order_from_broker(
                            order.client_order_id,
                            broker_order_id=b_ord.broker_order_id,
                            broker_state=b_ord.state,
                            broker_fills=b_ord.fills,
                            reason=f"Reconciled UNKNOWN_OUTCOME -> {b_ord.state.value} from broker snapshot",
                        )
                        unknown_resolved += 1
                        breaks.append(
                            ReconciliationBreak(
                                break_id=f"BRK-{uuid.uuid4().hex[:10].upper()}",
                                break_type=ReconciliationBreakType.UNKNOWN_ORDER_RESOLVED,
                                severity=ReconciliationBreakSeverity.INFO,
                                entity_id=order.client_order_id,
                                local_value=order.state.value,
                                broker_value=b_ord.state.value,
                                delta=f"synced_fills={len(b_ord.fills)}",
                                resolution=f"Synced broker state {b_ord.state.value} and updated ledger/reservation",
                            )
                        )
                    else:
                        # Order never reached broker -> safe to mark REJECTED and release reservation
                        self.oms.resolve_order_from_broker(
                            order.client_order_id,
                            broker_order_id=None,
                            broker_state=OrderState.REJECTED,
                            broker_fills=[],
                            reason="Order absent from broker snapshot during reconciliation; releasing reservation",
                        )
                        unknown_resolved += 1
                        breaks.append(
                            ReconciliationBreak(
                                break_id=f"BRK-{uuid.uuid4().hex[:10].upper()}",
                                break_type=ReconciliationBreakType.UNKNOWN_ORDER_RESOLVED,
                                severity=ReconciliationBreakSeverity.INFO,
                                entity_id=order.client_order_id,
                                local_value=order.state.value,
                                broker_value="ABSENT",
                                delta="never_landed",
                                resolution="Marked REJECTED and released reserved exposure",
                            )
                        )

            # Refresh local state after Phase 1 resolutions
            local_orders = self.store.list_orders()
            local_orders_by_cid = {o.client_order_id: o for o in local_orders}

            # Phase 2: Order state comparison
            for cid, b_ord in broker_orders_by_cid.items():
                l_ord = local_orders_by_cid.get(cid)
                if l_ord is None:
                    breaks.append(
                        ReconciliationBreak(
                            break_id=f"BRK-{uuid.uuid4().hex[:10].upper()}",
                            break_type=ReconciliationBreakType.ORPHAN_BROKER_ORDER,
                            severity=ReconciliationBreakSeverity.CRITICAL,
                            entity_id=cid,
                            local_value="MISSING",
                            broker_value=f"{b_ord.state.value} ({b_ord.symbol} {b_ord.quantity})",
                            delta=str(b_ord.filled_quantity),
                            resolution="Manual operator investigation required",
                        )
                    )
                    continue
                if l_ord.state != b_ord.state or l_ord.filled_quantity != b_ord.filled_quantity:
                    breaks.append(
                        ReconciliationBreak(
                            break_id=f"BRK-{uuid.uuid4().hex[:10].upper()}",
                            break_type=ReconciliationBreakType.ORDER_STATE_MISMATCH,
                            severity=ReconciliationBreakSeverity.CRITICAL,
                            entity_id=cid,
                            local_value=f"{l_ord.state.value}:{l_ord.filled_quantity}",
                            broker_value=f"{b_ord.state.value}:{b_ord.filled_quantity}",
                            delta=str(b_ord.filled_quantity - l_ord.filled_quantity),
                            resolution="Order state divergence between OMS and broker",
                        )
                    )

            for cid, l_ord in local_orders_by_cid.items():
                if l_ord.state in {OrderState.RISK_REJECTED, OrderState.REJECTED}:
                    continue
                if cid not in broker_orders_by_cid:
                    breaks.append(
                        ReconciliationBreak(
                            break_id=f"BRK-{uuid.uuid4().hex[:10].upper()}",
                            break_type=ReconciliationBreakType.MISSING_BROKER_ORDER,
                            severity=ReconciliationBreakSeverity.CRITICAL,
                            entity_id=cid,
                            local_value=l_ord.state.value,
                            broker_value="MISSING",
                            delta=str(l_ord.quantity),
                            resolution="Order present in OMS but missing from broker",
                        )
                    )

            # Phase 3: Position comparison
            local_positions = self.ledger.positions_map()
            broker_positions = {k: v for k, v in snap.positions.items() if v != 0}
            all_symbols = sorted(set(local_positions.keys()) | set(broker_positions.keys()))
            for sym in all_symbols:
                lq = local_positions.get(sym, Decimal("0"))
                bq = broker_positions.get(sym, Decimal("0"))
                if lq != bq:
                    breaks.append(
                        ReconciliationBreak(
                            break_id=f"BRK-{uuid.uuid4().hex[:10].upper()}",
                            break_type=ReconciliationBreakType.POSITION_QTY_BREAK,
                            severity=ReconciliationBreakSeverity.CRITICAL,
                            entity_id=sym,
                            local_value=str(lq),
                            broker_value=str(bq),
                            delta=str(bq - lq),
                            resolution="Position quantity mismatch between ledger and broker",
                        )
                    )

            # Phase 4: Cash comparison
            local_cash = self.ledger.cash_balance()
            broker_cash = snap.cash_balance.quantize(Decimal("0.0001"))
            cash_delta = (broker_cash - local_cash).quantize(Decimal("0.0001"))
            if abs(cash_delta) > CASH_TOLERANCE:
                breaks.append(
                    ReconciliationBreak(
                        break_id=f"BRK-{uuid.uuid4().hex[:10].upper()}",
                        break_type=ReconciliationBreakType.CASH_BALANCE_BREAK,
                        severity=ReconciliationBreakSeverity.CRITICAL,
                        entity_id=snap.account_id,
                        local_value=str(local_cash),
                        broker_value=str(broker_cash),
                        delta=str(cash_delta),
                        resolution="Cash balance mismatch exceeds tolerance ($0.01)",
                    )
                )

            # Phase 5: Reserved exposure consistency invariant
            active_res = {r.client_order_id: r for r in self.risk_engine.active_reservations()}
            for cid, l_ord in local_orders_by_cid.items():
                if l_ord.state.is_terminal and cid in active_res:
                    breaks.append(
                        ReconciliationBreak(
                            break_id=f"BRK-{uuid.uuid4().hex[:10].upper()}",
                            break_type=ReconciliationBreakType.RESERVED_EXPOSURE_DRIFT,
                            severity=ReconciliationBreakSeverity.WARNING,
                            entity_id=cid,
                            local_value=f"active_reservation={active_res[cid].reserved_notional}",
                            broker_value=f"terminal_state={l_ord.state.value}",
                            delta=str(active_res[cid].reserved_notional),
                            resolution="Stale active reservation on terminal order",
                        )
                    )
                elif l_ord.state.holds_reservation and cid not in active_res:
                    breaks.append(
                        ReconciliationBreak(
                            break_id=f"BRK-{uuid.uuid4().hex[:10].upper()}",
                            break_type=ReconciliationBreakType.RESERVED_EXPOSURE_DRIFT,
                            severity=ReconciliationBreakSeverity.CRITICAL,
                            entity_id=cid,
                            local_value="missing_reservation",
                            broker_value=f"open_state={l_ord.state.value}",
                            delta=str(l_ord.remaining_quantity),
                            resolution="Open/unknown order missing active risk reservation",
                        )
                    )

            critical_breaks = [
                b for b in breaks if b.severity == ReconciliationBreakSeverity.CRITICAL
            ]
            matched = len(critical_breaks) == 0
            kill_triggered = False

            if not matched and self.risk_engine.limits.halt_on_critical_reconciliation_break:
                self.risk_engine.engage_kill_switch(
                    scope=KillSwitchScope.GLOBAL,
                    target="ALL",
                    reason=f"Critical reconciliation break detected ({len(critical_breaks)} breaks)",
                    actor=actor,
                    actor_role=Role.RISK_OFFICER,
                    requires_dual_release=True,
                )
                kill_triggered = True

            report = ReconciliationReport(
                reconciliation_id=f"REC-{uuid.uuid4().hex[:12].upper()}",
                account_id=snap.account_id,
                run_at_utc=utc_now(),
                matched=matched,
                unknown_orders_resolved=unknown_resolved,
                local_cash=local_cash,
                broker_cash=broker_cash,
                cash_delta=cash_delta,
                local_positions=local_positions,
                broker_positions=broker_positions,
                breaks=breaks,
                kill_switch_triggered=kill_triggered,
            )
            self.store.save_reconciliation_report(report)
            self.store.append_audit(
                event_type="RECONCILIATION_COMPLETED",
                actor=actor,
                actor_role=Role.RISK_OFFICER,
                entity_type="RECONCILIATION",
                entity_id=report.reconciliation_id,
                payload={
                    "matched": matched,
                    "unknown_orders_resolved": unknown_resolved,
                    "critical_break_count": len(critical_breaks),
                    "cash_delta": str(cash_delta),
                    "kill_switch_triggered": kill_triggered,
                },
            )
            self.telemetry.inc("reconciliations_total")
            if not matched:
                self.telemetry.inc("reconciliations_failed")
            return report
