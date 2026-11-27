"""Deterministic pre-trade risk gate with reserved exposure tracking and kill switch controls."""

from __future__ import annotations

import uuid
from collections import defaultdict, deque
from datetime import datetime, timedelta
from decimal import Decimal

from trading_platform.domain.config import (
    ExecutionPolicyConfig,
    RiskLimitsConfig,
    validate_live_enablement,
)
from trading_platform.domain.errors import AuthorizationError, LiveModeBlockedError
from trading_platform.domain.events import (
    ExecutionMode,
    KillSwitchScope,
    KillSwitchState,
    MarketTick,
    OrderIntent,
    OrderRecord,
    RiskCheckResult,
    RiskReservation,
    RiskRuleOutcome,
    Role,
    Side,
    utc_now,
)
from trading_platform.ledger.ledger_service import LedgerService
from trading_platform.observability.telemetry import TelemetryRecorder
from trading_platform.storage.sqlite_store import SQLiteStore

KILL_SWITCH_ENGAGE_ROLES = {Role.OPERATOR, Role.RISK_OFFICER, Role.COMPLIANCE, Role.CTO}
DUAL_RELEASE_APPROVER_ROLES = {Role.RISK_OFFICER, Role.COMPLIANCE, Role.CTO}


class RiskEngine:
    """Evaluates deterministic pre-trade rules, maintains open/unknown order exposure reservations, and enforces kill switches."""

    def __init__(
        self,
        *,
        store: SQLiteStore,
        ledger: LedgerService,
        policy: ExecutionPolicyConfig,
        limits: RiskLimitsConfig,
        telemetry: TelemetryRecorder,
    ) -> None:
        self.store = store
        self.ledger = ledger
        self.policy = policy
        self.limits = limits
        self.telemetry = telemetry
        self._strategy_order_times: dict[str, deque[datetime]] = defaultdict(deque)

    # --- Kill Switch Controls ---

    def engage_kill_switch(
        self,
        *,
        scope: KillSwitchScope = KillSwitchScope.GLOBAL,
        target: str = "ALL",
        reason: str,
        actor: str,
        actor_role: Role,
        requires_dual_release: bool = False,
    ) -> KillSwitchState:
        if actor_role not in KILL_SWITCH_ENGAGE_ROLES:
            raise AuthorizationError(
                f"Role {actor_role.value} cannot engage kill switch",
                details={"actor": actor, "role": actor_role.value},
            )
        state = KillSwitchState(
            scope=scope,
            target=target,
            engaged=True,
            reason=reason,
            actor=actor,
            actor_role=actor_role,
            requires_dual_release=requires_dual_release,
            updated_at_utc=utc_now(),
        )
        self.store.save_kill_switch(state)
        self.store.append_audit(
            event_type="KILL_SWITCH_ENGAGED",
            actor=actor,
            actor_role=actor_role,
            entity_type="KILL_SWITCH",
            entity_id=f"{scope.value}:{target}",
            payload={
                "reason": reason,
                "requires_dual_release": requires_dual_release,
            },
        )
        self.telemetry.inc("kill_switch_engagements")
        self.telemetry.set_gauge("kill_switch_active", 1.0)
        return state

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
        if actor_role not in KILL_SWITCH_ENGAGE_ROLES:
            raise AuthorizationError(
                f"Role {actor_role.value} cannot release kill switch",
                details={"actor": actor, "role": actor_role.value},
            )
        current = next(
            (
                ks
                for ks in self.store.list_kill_switches()
                if ks.scope == scope and ks.target == target
            ),
            None,
        )
        if current and current.requires_dual_release:
            if not secondary_approver or secondary_role is None:
                raise AuthorizationError(
                    "Dual-control release required: provide secondary_approver and secondary_role",
                    details={"scope": scope.value, "target": target},
                )
            if secondary_approver == actor:
                raise AuthorizationError(
                    "Maker-checker violation: secondary_approver must differ from actor",
                    details={"actor": actor},
                )
            if secondary_role not in DUAL_RELEASE_APPROVER_ROLES:
                raise AuthorizationError(
                    f"Secondary role {secondary_role.value} cannot approve dual kill switch release",
                    details={"secondary_role": secondary_role.value},
                )

        state = KillSwitchState(
            scope=scope,
            target=target,
            engaged=False,
            reason=reason,
            actor=actor,
            actor_role=actor_role,
            requires_dual_release=False,
            updated_at_utc=utc_now(),
        )
        self.store.save_kill_switch(state)
        self.store.append_audit(
            event_type="KILL_SWITCH_RELEASED",
            actor=actor,
            actor_role=actor_role,
            entity_type="KILL_SWITCH",
            entity_id=f"{scope.value}:{target}",
            payload={
                "reason": reason,
                "secondary_approver": secondary_approver,
                "secondary_role": secondary_role.value if secondary_role else None,
            },
        )
        any_active = len(self.store.list_kill_switches(engaged_only=True)) > 0
        self.telemetry.set_gauge("kill_switch_active", 1.0 if any_active else 0.0)
        return state

    def active_kill_switches_for(self, strategy_id: str, symbol: str) -> list[KillSwitchState]:
        engaged = self.store.list_kill_switches(engaged_only=True)
        matched: list[KillSwitchState] = []
        for ks in engaged:
            if ks.scope == KillSwitchScope.GLOBAL and ks.target == "ALL":
                matched.append(ks)
            elif ks.scope == KillSwitchScope.STRATEGY and ks.target == strategy_id:
                matched.append(ks)
            elif ks.scope == KillSwitchScope.SYMBOL and ks.target == symbol:
                matched.append(ks)
        return matched

    # --- Reservation Accounting ---

    def active_reservations(self) -> list[RiskReservation]:
        return self.store.list_reservations(active_only=True)

    def total_reserved_notional(self) -> Decimal:
        return sum(
            (r.reserved_notional for r in self.active_reservations()),
            Decimal("0"),
        )

    def reserved_symbol_exposure(self, symbol: str) -> tuple[Decimal, Decimal, Decimal]:
        """Returns (buy_reserved_qty, sell_reserved_qty, total_reserved_notional) for symbol."""
        buy_qty = Decimal("0")
        sell_qty = Decimal("0")
        notional = Decimal("0")
        for r in self.active_reservations():
            if r.symbol != symbol:
                continue
            if r.side == Side.BUY:
                buy_qty += r.reserved_quantity
            else:
                sell_qty += r.reserved_quantity
            notional += r.reserved_notional
        return buy_qty, sell_qty, notional

    def reserve(self, intent: OrderIntent) -> RiskReservation:
        res = RiskReservation(
            reservation_id=f"RES-{uuid.uuid4().hex[:12].upper()}",
            client_order_id=intent.client_order_id,
            account_id=intent.account_id,
            strategy_id=intent.strategy_id,
            symbol=intent.symbol,
            side=intent.side,
            reserved_quantity=intent.quantity,
            reserved_notional=intent.notional,
            currency=intent.currency,
            active=True,
            retained_reason=None,
            created_at_utc=utc_now(),
            updated_at_utc=utc_now(),
        )
        self.store.save_reservation(res)
        self.telemetry.set_gauge("reserved_notional_total", float(self.total_reserved_notional()))
        return res

    def update_reservation_for_order(self, order: OrderRecord) -> RiskReservation | None:
        existing = self.store.get_reservation(order.client_order_id)
        if existing is None:
            return None
        if order.remaining_quantity <= 0 or order.state.is_terminal:
            updated = existing.model_copy(
                update={
                    "reserved_quantity": Decimal("0"),
                    "reserved_notional": Decimal("0"),
                    "active": False,
                    "retained_reason": None,
                    "updated_at_utc": utc_now(),
                }
            )
        else:
            ref_px = order.limit_price if order.limit_price is not None else order.reference_price
            rem_notional = (order.remaining_quantity * ref_px).quantize(Decimal("0.0001"))
            updated = existing.model_copy(
                update={
                    "reserved_quantity": order.remaining_quantity,
                    "reserved_notional": rem_notional,
                    "active": True,
                    "updated_at_utc": utc_now(),
                }
            )
        self.store.save_reservation(updated)
        self.telemetry.set_gauge("reserved_notional_total", float(self.total_reserved_notional()))
        return updated

    def retain_reservation_unknown(self, client_order_id: str, reason: str) -> RiskReservation | None:
        """Retains active reservation when broker order outcome is uncertain (UNKNOWN_OUTCOME)."""
        existing = self.store.get_reservation(client_order_id)
        if existing is None:
            return None
        updated = existing.model_copy(
            update={
                "active": True,
                "retained_reason": f"UNKNOWN_OUTCOME: {reason}",
                "updated_at_utc": utc_now(),
            }
        )
        self.store.save_reservation(updated)
        self.telemetry.inc("reservations_retained_unknown")
        self.telemetry.set_gauge("reserved_notional_total", float(self.total_reserved_notional()))
        return updated

    def release_reservation(self, client_order_id: str, reason: str) -> RiskReservation | None:
        existing = self.store.get_reservation(client_order_id)
        if existing is None:
            return None
        updated = existing.model_copy(
            update={
                "reserved_quantity": Decimal("0"),
                "reserved_notional": Decimal("0"),
                "active": False,
                "retained_reason": f"RELEASED: {reason}",
                "updated_at_utc": utc_now(),
            }
        )
        self.store.save_reservation(updated)
        self.telemetry.set_gauge("reserved_notional_total", float(self.total_reserved_notional()))
        return updated

    # --- Deterministic Pre-Trade Evaluation ---

    def evaluate(
        self,
        intent: OrderIntent,
        *,
        mark_prices: dict[str, Decimal],
        reference_tick: MarketTick | None = None,
        now_utc: datetime | None = None,
        record_rate: bool = True,
    ) -> RiskCheckResult:
        with self.telemetry.span(
            "risk.evaluate",
            attributes={
                "client_order_id": intent.client_order_id,
                "symbol": intent.symbol,
                "side": intent.side.value,
            },
            metric_name="risk_check_latency_ms",
        ):
            now = now_utc or utc_now()
            outcomes: list[RiskRuleOutcome] = []

            # R01: Execution Policy Gate (fail closed)
            r01_passed = False
            r01_reason = ""
            if self.policy.execution_mode == ExecutionMode.DISABLED:
                r01_reason = "Execution policy is DISABLED"
            elif self.policy.execution_mode == ExecutionMode.PAPER:
                if self.policy.allow_paper_execution:
                    r01_passed = True
                    r01_reason = f"Paper execution permitted by {self.policy.policy_id}"
                else:
                    r01_reason = "Paper execution disabled by policy"
            elif self.policy.execution_mode == ExecutionMode.SHADOW:
                if self.policy.allow_shadow_mode:
                    r01_passed = True
                    r01_reason = f"Shadow evaluation permitted by {self.policy.policy_id}"
                else:
                    r01_reason = "Shadow mode disabled by policy"
            elif self.policy.execution_mode == ExecutionMode.LIVE:
                try:
                    validate_live_enablement(self.policy, now_utc=now)
                    r01_passed = True
                    r01_reason = "Live execution approvals verified"
                except LiveModeBlockedError as exc:
                    r01_reason = f"{exc.message} ({'; '.join(exc.details.get('gaps', []))})"
            outcomes.append(
                RiskRuleOutcome(
                    rule_id="R01_EXECUTION_POLICY",
                    passed=r01_passed,
                    reason=r01_reason,
                    observed_value=self.policy.execution_mode.value,
                    limit_value="PAPER|SHADOW (LIVE gated)",
                )
            )

            # R02: Kill Switch
            active_switches = self.active_kill_switches_for(intent.strategy_id, intent.symbol)
            if active_switches:
                reasons = "; ".join(
                    f"{s.scope.value}:{s.target} ({s.reason})" for s in active_switches
                )
                outcomes.append(
                    RiskRuleOutcome(
                        rule_id="R02_KILL_SWITCH",
                        passed=False,
                        reason=f"Kill switch engaged: {reasons}",
                        observed_value="ENGAGED",
                        limit_value="DISENGAGED",
                    )
                )
            else:
                outcomes.append(
                    RiskRuleOutcome(
                        rule_id="R02_KILL_SWITCH",
                        passed=True,
                        reason="No active kill switch",
                        observed_value="DISENGAGED",
                        limit_value="DISENGAGED",
                    )
                )

            # R03: Symbol Allowlist
            allowed = intent.symbol in self.limits.allowed_symbols
            outcomes.append(
                RiskRuleOutcome(
                    rule_id="R03_SYMBOL_ALLOWLIST",
                    passed=allowed,
                    reason=(
                        f"Symbol {intent.symbol} is allowlisted"
                        if allowed
                        else f"Symbol {intent.symbol} not in allowlist"
                    ),
                    observed_value=intent.symbol,
                    limit_value=",".join(self.limits.allowed_symbols),
                )
            )

            # R04: Price Collar (Fat-Finger Protection)
            ref_mid = (
                reference_tick.mid
                if reference_tick is not None
                else mark_prices.get(intent.symbol, intent.reference_price)
            )
            bps_diff = (abs(intent.effective_price - ref_mid) / ref_mid) * Decimal("10000")
            collar_ok = bps_diff <= self.limits.max_price_collar_bps
            outcomes.append(
                RiskRuleOutcome(
                    rule_id="R04_PRICE_COLLAR",
                    passed=collar_ok,
                    reason=(
                        f"Price deviation {bps_diff.quantize(Decimal('0.01'))} bps within collar"
                        if collar_ok
                        else f"Fat-finger price collar breach: {bps_diff.quantize(Decimal('0.01'))} bps > {self.limits.max_price_collar_bps} bps"
                    ),
                    observed_value=str(bps_diff.quantize(Decimal("0.01"))),
                    limit_value=str(self.limits.max_price_collar_bps),
                )
            )

            # R05: Max Order Quantity
            qty_ok = intent.quantity <= self.limits.max_order_quantity
            outcomes.append(
                RiskRuleOutcome(
                    rule_id="R05_MAX_ORDER_QTY",
                    passed=qty_ok,
                    reason=(
                        "Order quantity within limit"
                        if qty_ok
                        else f"Order qty {intent.quantity} exceeds max {self.limits.max_order_quantity}"
                    ),
                    observed_value=str(intent.quantity),
                    limit_value=str(self.limits.max_order_quantity),
                )
            )

            # R06: Max Order Notional
            notional_ok = intent.notional <= self.limits.max_order_notional
            outcomes.append(
                RiskRuleOutcome(
                    rule_id="R06_MAX_ORDER_NOTIONAL",
                    passed=notional_ok,
                    reason=(
                        "Order notional within limit"
                        if notional_ok
                        else f"Order notional {intent.notional} exceeds max {self.limits.max_order_notional}"
                    ),
                    observed_value=str(intent.notional),
                    limit_value=str(self.limits.max_order_notional),
                )
            )

            # R07: Max Symbol Position & Notional (INCLUDING OPEN & UNKNOWN_OUTCOME RESERVATIONS)
            pos = self.store.get_position(intent.account_id, intent.symbol)
            current_qty = pos.quantity if pos else Decimal("0")
            buy_res_qty, sell_res_qty, sym_res_notional = self.reserved_symbol_exposure(
                intent.symbol
            )
            if intent.side == Side.BUY:
                worst_case_qty = abs(current_qty + buy_res_qty + intent.quantity)
            else:
                worst_case_qty = abs(current_qty - sell_res_qty - intent.quantity)

            current_sym_notional = abs(current_qty) * ref_mid
            worst_case_sym_notional = current_sym_notional + sym_res_notional + intent.notional
            sym_ok = (
                worst_case_qty <= self.limits.max_symbol_position_qty
                and worst_case_sym_notional <= self.limits.max_symbol_notional
            )
            outcomes.append(
                RiskRuleOutcome(
                    rule_id="R07_SYMBOL_POSITION_LIMIT",
                    passed=sym_ok,
                    reason=(
                        "Projected symbol exposure (incl. reservations) within limit"
                        if sym_ok
                        else (
                            f"Symbol exposure breach (incl. reserved exposure): "
                            f"qty={worst_case_qty}/{self.limits.max_symbol_position_qty}, "
                            f"notional={worst_case_sym_notional.quantize(Decimal('0.01'))}/{self.limits.max_symbol_notional}"
                        )
                    ),
                    observed_value=f"qty={worst_case_qty},notional={worst_case_sym_notional.quantize(Decimal('0.01'))}",
                    limit_value=f"qty={self.limits.max_symbol_position_qty},notional={self.limits.max_symbol_notional}",
                )
            )

            # R08: Max Gross Portfolio Notional (INCLUDING ALL ACTIVE RESERVATIONS)
            gross_positions = self.ledger.gross_position_notional(mark_prices)
            total_reserved = self.total_reserved_notional()
            projected_gross = gross_positions + total_reserved + intent.notional
            gross_ok = projected_gross <= self.limits.max_gross_notional
            outcomes.append(
                RiskRuleOutcome(
                    rule_id="R08_GROSS_EXPOSURE_LIMIT",
                    passed=gross_ok,
                    reason=(
                        "Projected gross portfolio notional within limit"
                        if gross_ok
                        else (
                            f"Gross exposure breach: positions({gross_positions}) + "
                            f"reserved({total_reserved}) + order({intent.notional}) = "
                            f"{projected_gross.quantize(Decimal('0.01'))} > {self.limits.max_gross_notional}"
                        )
                    ),
                    observed_value=str(projected_gross.quantize(Decimal("0.01"))),
                    limit_value=str(self.limits.max_gross_notional),
                )
            )

            # R09: Daily Loss Limit
            daily_pnl = self.ledger.net_daily_pnl(mark_prices)
            loss_ok = daily_pnl >= -self.limits.max_daily_loss
            outcomes.append(
                RiskRuleOutcome(
                    rule_id="R09_DAILY_LOSS_LIMIT",
                    passed=loss_ok,
                    reason=(
                        f"Daily PnL {daily_pnl} within loss threshold"
                        if loss_ok
                        else f"Daily loss breach: PnL {daily_pnl} < -{self.limits.max_daily_loss}"
                    ),
                    observed_value=str(daily_pnl),
                    limit_value=str(-self.limits.max_daily_loss),
                )
            )

            # R10: Strategy Order Rate Limit (sliding 60s window)
            window = self._strategy_order_times[intent.strategy_id]
            cutoff = now - timedelta(seconds=60)
            while window and window[0] < cutoff:
                window.popleft()
            rate_ok = len(window) < self.limits.max_orders_per_minute_per_strategy
            outcomes.append(
                RiskRuleOutcome(
                    rule_id="R10_ORDER_RATE_LIMIT",
                    passed=rate_ok,
                    reason=(
                        f"Order rate {len(window)}/min within limit"
                        if rate_ok
                        else f"Rate limit exceeded for {intent.strategy_id}: {len(window)}/{self.limits.max_orders_per_minute_per_strategy} per min"
                    ),
                    observed_value=str(len(window)),
                    limit_value=str(self.limits.max_orders_per_minute_per_strategy),
                )
            )

            blocking = [f"{o.rule_id}: {o.reason}" for o in outcomes if not o.passed]
            approved = len(blocking) == 0

            if approved and record_rate:
                window.append(now)

            self.telemetry.inc("risk_checks_total")
            if approved:
                self.telemetry.inc("risk_checks_approved")
            else:
                self.telemetry.inc("risk_checks_rejected")

            return RiskCheckResult(
                check_id=f"RCK-{uuid.uuid4().hex[:12].upper()}",
                client_order_id=intent.client_order_id,
                approved=approved,
                policy_version=self.limits.policy_version,
                checked_at_utc=now,
                outcomes=outcomes,
                blocking_reasons=blocking,
            )
