"""Top-level vertical-slice TradingPlatform orchestrator."""

from __future__ import annotations

import uuid
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any

from trading_platform.broker.adapter import BrokerAdapter
from trading_platform.broker.paper_broker import PaperBrokerAdapter
from trading_platform.domain.config import PlatformConfig, validate_live_enablement
from trading_platform.domain.errors import (
    ClockSkewError,
    DataEntitlementError,
    LiveModeBlockedError,
    StaleFeedError,
)
from trading_platform.domain.events import (
    BrokerAccountSnapshot,
    ExecutionMode,
    FeedEntitlement,
    KillSwitchScope,
    KillSwitchState,
    MarketTick,
    OrderIntent,
    OrderRecord,
    OrderState,
    ReconciliationReport,
    RiskCheckResult,
    Role,
    ShadowDecisionRecord,
    utc_now,
)
from trading_platform.ledger.ledger_service import LedgerService
from trading_platform.observability.telemetry import TelemetryRecorder
from trading_platform.oms.order_manager import OrderManager
from trading_platform.reconciliation.reconciler import BrokerReconciler
from trading_platform.risk.risk_engine import RiskEngine
from trading_platform.storage.sqlite_store import SQLiteStore
from trading_platform.strategy.market_data import MarketDataGuard
from trading_platform.strategy.scheduler import StrategyScheduler
from trading_platform.strategy.strategies import build_strategy


@dataclass
class TickProcessingResult:
    tick_id: str
    symbol: str
    accepted: bool
    rejection_code: str | None = None
    rejection_message: str | None = None
    orders: list[OrderRecord] = field(default_factory=list)
    shadow_decisions: list[ShadowDecisionRecord] = field(default_factory=list)


class TradingPlatform:
    """Integrated paper-first algorithmic trading platform orchestrating all core components."""

    def __init__(
        self,
        config: PlatformConfig,
        *,
        broker: BrokerAdapter | None = None,
        store: SQLiteStore | None = None,
    ) -> None:
        self.config = config
        self.telemetry = TelemetryRecorder()
        self.store = store or SQLiteStore(config.db_path)
        self.ledger = LedgerService(
            self.store,
            account_id=config.account_id,
            base_currency=config.base_currency,
        )
        self.ledger.initialize_capital(config.initial_cash)

        self.broker: BrokerAdapter = broker or PaperBrokerAdapter(
            config=config.broker,
            account_id=config.account_id,
            initial_cash=config.initial_cash,
            base_currency=config.base_currency,
        )
        self.risk_engine = RiskEngine(
            store=self.store,
            ledger=self.ledger,
            policy=config.execution_policy,
            limits=config.risk_limits,
            telemetry=self.telemetry,
        )
        self.oms = OrderManager(
            store=self.store,
            risk_engine=self.risk_engine,
            ledger=self.ledger,
            broker=self.broker,
            telemetry=self.telemetry,
        )
        self.market_data = MarketDataGuard(
            config=config.feed,
            execution_mode=config.execution_policy.execution_mode,
            telemetry=self.telemetry,
        )
        self.scheduler = StrategyScheduler(
            account_id=config.account_id,
            base_currency=config.base_currency,
        )
        for strat_cfg in config.strategies:
            self.scheduler.register(build_strategy(strat_cfg))

        self.reconciler = BrokerReconciler(
            store=self.store,
            oms=self.oms,
            ledger=self.ledger,
            risk_engine=self.risk_engine,
            broker=self.broker,
            telemetry=self.telemetry,
        )
        self.store.append_audit(
            event_type="PLATFORM_STARTED",
            actor="SYSTEM",
            actor_role=Role.OPERATOR,
            entity_type="PLATFORM",
            entity_id=config.account_id,
            payload={
                "execution_mode": config.execution_policy.execution_mode.value,
                "policy_id": config.execution_policy.policy_id,
                "config_version": config.config_version,
            },
        )

    def close(self) -> None:
        self.store.close()

    def register_entitlement(self, entitlement: FeedEntitlement) -> None:
        self.market_data.register_entitlement(entitlement)

    def process_tick(
        self,
        tick: MarketTick,
        *,
        now_utc: datetime | None = None,
    ) -> TickProcessingResult:
        now = now_utc or utc_now()
        with self.telemetry.span(
            "platform.process_tick",
            attributes={"tick_id": tick.tick_id, "symbol": tick.symbol},
            metric_name="tick_to_decision_latency_ms",
        ):
            try:
                self.market_data.validate_tick(tick, now_utc=now)
            except (StaleFeedError, ClockSkewError, DataEntitlementError) as exc:
                self.store.append_audit(
                    event_type="MARKET_TICK_REJECTED",
                    actor="MARKET_DATA_GUARD",
                    actor_role=Role.OPERATOR,
                    entity_type="TICK",
                    entity_id=tick.tick_id,
                    payload=exc.to_dict(),
                )
                return TickProcessingResult(
                    tick_id=tick.tick_id,
                    symbol=tick.symbol,
                    accepted=False,
                    rejection_code=exc.code,
                    rejection_message=exc.message,
                )

            intents = self.scheduler.on_tick(tick)
            marks = self.market_data.latest_marks()
            mode = self.config.execution_policy.execution_mode

            if mode == ExecutionMode.SHADOW:
                shadow_records: list[ShadowDecisionRecord] = []
                for _sig, intent in intents:
                    risk_res = self.risk_engine.evaluate(
                        intent,
                        mark_prices=marks,
                        reference_tick=tick,
                        now_utc=now,
                        record_rate=False,
                    )
                    rec = ShadowDecisionRecord(
                        shadow_id=f"SHD-{uuid.uuid4().hex[:12].upper()}",
                        tick_id=tick.tick_id,
                        strategy_id=intent.strategy_id,
                        symbol=intent.symbol,
                        side=intent.side,
                        quantity=intent.quantity,
                        reference_price=intent.reference_price,
                        risk_approved=risk_res.approved,
                        blocking_reasons=risk_res.blocking_reasons,
                        dispatched_to_broker=False,
                        recorded_at_utc=now,
                    )
                    self.store.save_shadow_decision(rec)
                    self.telemetry.inc("shadow_decisions_recorded")
                    shadow_records.append(rec)
                return TickProcessingResult(
                    tick_id=tick.tick_id,
                    symbol=tick.symbol,
                    accepted=True,
                    shadow_decisions=shadow_records,
                )

            orders: list[OrderRecord] = []
            for _sig, intent in intents:
                order, _risk, _replayed = self.oms.submit_order(
                    intent,
                    mark_prices=marks,
                    reference_tick=tick,
                    now_utc=now,
                )
                orders.append(order)

            return TickProcessingResult(
                tick_id=tick.tick_id,
                symbol=tick.symbol,
                accepted=True,
                orders=orders,
            )

    def submit_manual_intent(
        self,
        intent: OrderIntent,
        *,
        reference_tick: MarketTick | None = None,
        now_utc: datetime | None = None,
    ) -> tuple[OrderRecord, RiskCheckResult | None, bool]:
        tick = reference_tick or self.market_data.latest_tick(intent.symbol)
        marks = self.market_data.latest_marks()
        if intent.symbol not in marks:
            marks[intent.symbol] = intent.reference_price
        return self.oms.submit_order(
            intent,
            mark_prices=marks,
            reference_tick=tick,
            now_utc=now_utc,
        )

    def engage_kill_switch(
        self,
        *,
        scope: KillSwitchScope = KillSwitchScope.GLOBAL,
        target: str = "ALL",
        reason: str,
        actor: str,
        actor_role: Role,
        cancel_open_orders: bool = True,
        requires_dual_release: bool = False,
    ) -> tuple[KillSwitchState, list[OrderRecord]]:
        state = self.risk_engine.engage_kill_switch(
            scope=scope,
            target=target,
            reason=reason,
            actor=actor,
            actor_role=actor_role,
            requires_dual_release=requires_dual_release,
        )
        canceled: list[OrderRecord] = []
        if cancel_open_orders and self.broker.is_connected():
            open_orders = self.store.list_orders(
                states={OrderState.ACKED, OrderState.PARTIALLY_FILLED}
            )
            for o in open_orders:
                if (
                    scope == KillSwitchScope.GLOBAL
                    or (scope == KillSwitchScope.STRATEGY and o.strategy_id == target)
                    or (scope == KillSwitchScope.SYMBOL and o.symbol == target)
                ):
                    canceled.append(
                        self.oms.cancel_order(
                            o.client_order_id,
                            actor=actor,
                            reason=f"Kill switch engaged ({reason})",
                        )
                    )
        return state, canceled

    def release_kill_switch(
        self,
        *,
        scope: KillSwitchScope = KillSwitchScope.GLOBAL,
        target: str = "ALL",
        reason: str,
        actor: str,
        actor_role: Role,
        secondary_approver: str | None = None,
        secondary_role: Role | None = None,
    ) -> KillSwitchState:
        return self.risk_engine.release_kill_switch(
            scope=scope,
            target=target,
            reason=reason,
            actor=actor,
            actor_role=actor_role,
            secondary_approver=secondary_approver,
            secondary_role=secondary_role,
        )

    def reconcile(
        self,
        *,
        broker_snapshot: BrokerAccountSnapshot | None = None,
        actor: str = "RECONCILER",
    ) -> ReconciliationReport:
        return self.reconciler.reconcile(broker_snapshot=broker_snapshot, actor=actor)

    def recover_after_restart(self) -> dict[str, Any]:
        """Verifies durable state integrity on startup and reconciles uncertain orders."""
        audit_ok, audit_msg = self.store.verify_audit_chain()
        ledger_balanced = self.ledger.is_balanced()
        active_switches = self.store.list_kill_switches(engaged_only=True)
        open_orders = self.store.list_orders(
            states={
                OrderState.SUBMITTING,
                OrderState.ACKED,
                OrderState.PARTIALLY_FILLED,
                OrderState.CANCEL_PENDING,
                OrderState.UNKNOWN_OUTCOME,
            }
        )
        active_res = self.risk_engine.active_reservations()
        reconcile_report: ReconciliationReport | None = None
        if self.broker.is_connected():
            reconcile_report = self.reconciler.reconcile(actor="STARTUP_RECOVERY")

        summary = {
            "recovered_at_utc": utc_now().isoformat(),
            "audit_chain_valid": audit_ok,
            "audit_chain_message": audit_msg,
            "ledger_balanced": ledger_balanced,
            "active_kill_switches": len(active_switches),
            "open_orders_before_reconcile": len(open_orders),
            "active_reservations_before_reconcile": len(active_res),
            "reconciled": reconcile_report.matched if reconcile_report else False,
            "unknown_orders_resolved": (
                reconcile_report.unknown_orders_resolved if reconcile_report else 0
            ),
        }
        self.store.append_audit(
            event_type="PLATFORM_RECOVERED",
            actor="SYSTEM",
            actor_role=Role.OPERATOR,
            entity_type="PLATFORM",
            entity_id=self.config.account_id,
            payload=summary,
        )
        return summary

    def check_live_gate(self) -> dict[str, Any]:
        try:
            validate_live_enablement(self.config.execution_policy)
            return {"live_allowed": True, "gaps": []}
        except LiveModeBlockedError as exc:
            return {
                "live_allowed": False,
                "policy_id": self.config.execution_policy.policy_id,
                "gaps": exc.details.get("gaps", [exc.message]),
            }

    def status_summary(self) -> dict[str, Any]:
        marks = self.market_data.latest_marks()
        orders = self.store.list_orders()
        reservations = self.risk_engine.active_reservations()
        switches = self.store.list_kill_switches(engaged_only=True)
        recons = self.store.list_reconciliation_reports()
        audit_ok, audit_msg = self.store.verify_audit_chain()

        return {
            "account_id": self.config.account_id,
            "environment": self.config.environment,
            "execution_mode": self.config.execution_policy.execution_mode.value,
            "policy_id": self.config.execution_policy.policy_id,
            "live_gate": self.check_live_gate(),
            "broker_connected": self.broker.is_connected(),
            "kill_switches_engaged": [s.model_dump(mode="json") for s in switches],
            "ledger": {
                "balanced": self.ledger.is_balanced(),
                "cash_balance": str(self.ledger.cash_balance()),
                "realized_pnl": str(self.ledger.total_realized_pnl()),
                "unrealized_pnl": str(self.ledger.unrealized_pnl(marks)),
                "commissions": str(self.ledger.total_commissions()),
                "net_daily_pnl": str(self.ledger.net_daily_pnl(marks)),
                "positions": {k: str(v) for k, v in self.ledger.positions_map().items()},
            },
            "orders": {
                "total": len(orders),
                "open": sum(1 for o in orders if o.state.holds_reservation),
                "unknown_outcome": sum(1 for o in orders if o.state == OrderState.UNKNOWN_OUTCOME),
                "filled": sum(1 for o in orders if o.state == OrderState.FILLED),
                "risk_rejected": sum(1 for o in orders if o.state == OrderState.RISK_REJECTED),
            },
            "reservations": {
                "active_count": len(reservations),
                "total_reserved_notional": str(self.risk_engine.total_reserved_notional()),
                "items": [r.model_dump(mode="json") for r in reservations],
            },
            "reconciliation": {
                "run_count": len(recons),
                "latest": recons[-1].model_dump(mode="json") if recons else None,
            },
            "shadow_decisions_count": len(self.store.list_shadow_decisions()),
            "audit_chain": {"valid": audit_ok, "message": audit_msg},
            "strategies": self.scheduler.list_strategies(),
            "telemetry": self.telemetry.snapshot(),
        }
