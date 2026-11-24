"""Application service: intent -> proposal -> risk decision -> approval -> order.

This is the composition root for the logical services described in the README
(agent-api, tool-gateway, policy-service, oms, audit-service). They run in one process
but only talk through the interfaces wired here.
"""

from __future__ import annotations

from datetime import timedelta
from decimal import ROUND_DOWN, ROUND_HALF_UP, Decimal, InvalidOperation

from .approvals import ApprovalService
from .audit import AuditLog
from .broker import BrokerUnavailable, PaperBroker, ReadOnlyBroker
from .clock import Clock, SystemClock
from .config import Settings
from .db import Database
from .domain import (
    TICK,
    Approval,
    Decision,
    Order,
    OrderType,
    ParsedIntent,
    ProposalStatus,
    Quote,
    RiskDecision,
    Side,
    TradeIntent,
    TradeProposal,
    compute_proposal_hash,
    hash_of,
)
from .ids import IdFactory, random_id
from .marketdata import MarketDataUnavailable, SimulatedMarketData
from .metrics import Metrics
from .oms import OrderManager
from .planner import Planner, build_planner
from .policy import PolicyInputs, PolicyService
from .store import Store
from .tools import Principal, ToolGateway


class ServiceError(Exception):
    status = 400

    def __init__(self, code: str, message: str, **extra) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.extra = extra


class NotFound(ServiceError):
    status = 404


class Conflict(ServiceError):
    status = 409


class Unprocessable(ServiceError):
    status = 422


class Unavailable(ServiceError):
    status = 503


# Sanity bound on planner-supplied numbers. Real limits live in the policy gate; this only
# keeps absurd values out of the store.
MAX_INPUT_VALUE = Decimal("1000000000")


def _decimal(value: str | None, field: str) -> Decimal | None:
    if value is None:
        return None
    try:
        parsed = Decimal(str(value).replace(",", "").replace("$", "").strip())
    except InvalidOperation:
        raise Unprocessable("INVALID_INTENT", f"{field} is not a number: {value!r}") from None
    if not parsed.is_finite() or parsed <= 0:
        raise Unprocessable("INVALID_INTENT", f"{field} must be a positive number")
    if parsed > MAX_INPUT_VALUE:
        raise Unprocessable("INVALID_INTENT", f"{field} is implausibly large")
    return parsed


class TradingService:
    def __init__(
        self,
        settings: Settings,
        *,
        clock: Clock | None = None,
        new_id: IdFactory | None = None,
        planner: Planner | None = None,
        db: Database | None = None,
    ) -> None:
        self.settings = settings
        self.clock = clock or SystemClock()
        self.new_id = new_id or random_id
        self.db = db or Database(settings.db_path)
        self.store = Store(self.db)
        self.metrics = Metrics()
        self.audit = AuditLog(self.db, self.clock, self.new_id)
        self.market_data = SimulatedMarketData(self.clock, settings.universe, settings.sim_seed)
        self.broker = PaperBroker(
            self.db,
            self.market_data,
            self.clock,
            self.new_id,
            settings.starting_cash,
            settings.sim_max_fill_per_match,
        )
        self.broker_read = ReadOnlyBroker(self.broker)
        self.tools = ToolGateway(self.market_data, self.broker_read, self.audit)
        self.planner = planner or build_planner(settings.planner, settings.planner_model)
        self.policy = PolicyService(settings)
        self.approvals = ApprovalService(
            settings.approval_secret, settings.approval_ttl_seconds, self.new_id
        )
        self.oms = OrderManager(
            self.store,
            self.broker,  # the only write-capable broker handle in the system
            self.approvals,
            self._risk_check,
            self.audit,
            self.metrics,
            self.clock,
            settings.max_submit_attempts,
        )

    # -- intents ------------------------------------------------------------
    def create_intent(
        self, user_id: str, text: str, mode: str = "paper", correlation_id: str | None = None
    ) -> tuple[TradeIntent, TradeProposal]:
        correlation_id = correlation_id or self.new_id("cor")
        text = text.strip()
        if mode != "paper":
            self.audit.append(
                "intent.rejected",
                correlation_id=correlation_id,
                actor=user_id,
                payload={"reason": "MODE_NOT_SUPPORTED", "mode": mode},
            )
            raise ServiceError(
                "MODE_NOT_SUPPORTED", "only mode 'paper' is supported; no live trading exists"
            )
        if not text or len(text) > 2000:
            raise Unprocessable("INVALID_INTENT", "text must be 1-2000 characters")

        parsed = self.planner.plan(text, self.market_data.symbols)
        planner_name = self.planner.name
        try:
            fields = self._validate_parsed(parsed)
            quote = self._quote(user_id, fields["symbol"], correlation_id)
            if quote is None:
                raise Unavailable(
                    "MARKET_DATA_UNAVAILABLE", "cannot price a proposal without a quote; try again"
                )
            quantity, limit_price, reference = self._size(fields, quote)
        except ServiceError as exc:
            self.audit.append(
                "intent.rejected",
                correlation_id=correlation_id,
                actor=user_id,
                payload={"reason": exc.code, "detail": exc.message, "planner": planner_name},
            )
            raise

        now = self.clock.now()
        intent = TradeIntent(
            id=self.new_id("int"),
            correlation_id=correlation_id,
            user_id=user_id,
            text=text,
            mode=mode,
            symbol=fields["symbol"],
            side=fields["side"],
            notional=fields["notional"],
            quantity=fields["quantity"],
            order_type=fields["order_type"],
            limit_price=fields["limit_price"],
            rationale=(parsed.rationale or "")[:1000],
            planner=planner_name,
            created_at=now,
        )
        proposal_id = self.new_id("prp")
        proposal = TradeProposal(
            id=proposal_id,
            intent_id=intent.id,
            correlation_id=correlation_id,
            user_id=user_id,
            mode=mode,
            symbol=intent.symbol,
            side=intent.side,
            order_type=intent.order_type,
            quantity=quantity,
            limit_price=limit_price,
            reference_price=reference,
            quote_asof=quote.event_time,
            quote_received_at=quote.received_at,
            status=ProposalStatus.DRAFT,
            proposal_hash=compute_proposal_hash(
                proposal_id=proposal_id,
                intent_id=intent.id,
                user_id=user_id,
                mode=mode,
                symbol=intent.symbol,
                side=intent.side,
                order_type=intent.order_type,
                quantity=quantity,
                limit_price=limit_price,
                time_in_force="day",
            ),
            created_at=now,
            updated_at=now,
        )
        with self.db.transaction():
            self.db.insert("intents", intent)
            self.db.insert("proposals", proposal)
            self.audit.append(
                "intent.created",
                correlation_id=correlation_id,
                actor=user_id,
                payload={"intent_id": intent.id, "text": text, "planner": planner_name},
            )
            self.audit.append(
                "proposal.created",
                correlation_id=correlation_id,
                actor="agent-api",
                payload={
                    "proposal_id": proposal.id,
                    "intent_id": intent.id,
                    "symbol": proposal.symbol,
                    "side": proposal.side.value,
                    "order_type": proposal.order_type.value,
                    "quantity": proposal.quantity,
                    "limit_price": None if limit_price is None else str(limit_price),
                    "proposal_hash": proposal.proposal_hash,
                    "quote_asof": quote.event_time.isoformat(),
                },
            )
        return intent, proposal

    def _validate_parsed(self, parsed: ParsedIntent) -> dict:
        """Planner output is untrusted: re-validate every field."""
        if not parsed.actionable:
            raise Unprocessable(
                "INTENT_NOT_ACTIONABLE",
                parsed.clarification or "could not extract a single buy or sell instruction",
            )
        symbol = (parsed.symbol or "").strip().upper()
        if symbol not in self.settings.universe:
            raise Unprocessable(
                "SYMBOL_NOT_ALLOWED",
                f"{symbol or 'symbol'} is not tradable here; supported: "
                + ", ".join(sorted(self.settings.universe)),
            )
        if parsed.side is None:
            raise Unprocessable("INVALID_INTENT", "side (buy or sell) is required")
        notional = _decimal(parsed.notional, "notional")
        limit_price = _decimal(parsed.limit_price, "limit_price")
        quantity = parsed.quantity
        if (notional is None) == (quantity is None):
            raise Unprocessable("INVALID_INTENT", "give exactly one of a dollar amount or shares")
        if quantity is not None and not 0 < quantity <= MAX_INPUT_VALUE:
            raise Unprocessable("INVALID_INTENT", "quantity must be a positive whole number")
        order_type = parsed.order_type or OrderType.LIMIT
        if order_type is OrderType.MARKET:
            limit_price = None
        elif limit_price is not None and limit_price % TICK != 0:
            raise Unprocessable("INVALID_INTENT", f"limit price must be on a {TICK} tick")
        return {
            "symbol": symbol,
            "side": parsed.side,
            "notional": notional,
            "quantity": quantity,
            "order_type": order_type,
            "limit_price": limit_price,
        }

    def _size(self, fields: dict, quote: Quote) -> tuple[int, Decimal | None, Decimal]:
        """Deterministic sizing. Returns (quantity, limit_price, reference_price)."""
        side, order_type = fields["side"], fields["order_type"]
        touch = quote.ask if side is Side.BUY else quote.bid
        limit_price = fields["limit_price"]
        if order_type is OrderType.LIMIT and limit_price is None:
            limit_price = touch  # marketable limit at the current touch
        if order_type is OrderType.LIMIT:
            sizing_price = limit_price
        else:
            collar = 1 + self.settings.market_order_collar_bps / Decimal(10000)
            sizing_price = (touch * collar).quantize(TICK, rounding=ROUND_HALF_UP)
        quantity = fields["quantity"]
        if quantity is None:
            quantity = int((fields["notional"] / sizing_price).to_integral_value(ROUND_DOWN))
            if quantity < 1:
                raise Unprocessable(
                    "NOTIONAL_TOO_SMALL",
                    f"${fields['notional']} buys less than one share at {sizing_price}; "
                    "fractional shares are not supported",
                )
        return quantity, limit_price, touch

    def _quote(self, user_id: str, symbol: str, correlation_id: str) -> Quote | None:
        try:
            return self.tools.call(
                "get_quote", Principal(user_id), correlation_id=correlation_id, symbol=symbol
            )
        except (MarketDataUnavailable, KeyError):
            return None

    # -- proposals ----------------------------------------------------------
    def get_proposal(self, user_id: str, proposal_id: str) -> TradeProposal:
        proposal = self.store.proposal(proposal_id)
        if proposal is None or proposal.user_id != user_id:
            raise NotFound("PROPOSAL_NOT_FOUND", "no such proposal")
        return proposal

    def proposal_view(self, user_id: str, proposal_id: str) -> dict:
        proposal = self.get_proposal(user_id, proposal_id)
        order = self.store.order_for_proposal(proposal_id)
        return {
            "proposal": proposal,
            "intent": self.store.intent(proposal.intent_id),
            "risk_decision": self.store.latest_risk_decision(proposal_id),
            "approval": self._public_approval(self.store.approval_for(proposal_id)),
            "order_id": order.client_order_id if order else None,
        }

    @staticmethod
    def _public_approval(approval: Approval | None) -> dict | None:
        return None if approval is None else approval.model_dump(mode="json", exclude={"signature"})

    def evaluate(self, user_id: str, proposal_id: str) -> RiskDecision:
        with self.db.lock:
            proposal = self.get_proposal(user_id, proposal_id)
            if proposal.status not in (
                ProposalStatus.DRAFT,
                ProposalStatus.RISK_PASSED,
                ProposalStatus.RISK_REJECTED,
            ):
                raise Conflict(
                    "PROPOSAL_NOT_EVALUABLE",
                    f"proposal is {proposal.status.value}; it can no longer be evaluated",
                )
            decision = self._risk_check(proposal, "pre_approval")
            self.store.set_proposal_status(
                proposal.id,
                ProposalStatus.RISK_PASSED if decision.passed else ProposalStatus.RISK_REJECTED,
                self.clock.now(),
            )
            return decision

    def _risk_check(self, proposal: TradeProposal, stage: str) -> RiskDecision:
        """Gather fresh inputs through read-only tools and run the deterministic gate."""
        now = self.clock.now()
        principal = Principal(proposal.user_id)
        cid = proposal.correlation_id
        quote = (
            self._quote(proposal.user_id, proposal.symbol, cid)
            if proposal.symbol in self.settings.universe
            else None
        )
        try:
            account = self.broker_read.get_account(proposal.user_id)
            open_orders = self.tools.call("get_open_orders", principal, correlation_id=cid)
        except BrokerUnavailable:
            account, open_orders = None, []
        recent = self.store.similar_live_proposals(
            proposal, now - timedelta(seconds=self.settings.duplicate_window_seconds)
        )
        checks = self.policy.evaluate(
            PolicyInputs(
                proposal=proposal,
                now=now,
                quote=quote,
                account=account,
                open_orders=open_orders,
                recent_similar=recent,
            )
        )
        violations = [c for c in checks if not c.passed]
        decision = RiskDecision(
            id=self.new_id("rsk"),
            proposal_id=proposal.id,
            policy_version=self.policy.version,
            stage=stage,
            passed=not violations,
            violations=violations,
            checks=checks,
            evaluated_at=now,
        )
        if quote is not None:
            self.metrics.quote_staleness.observe(max(0.0, (now - quote.event_time).total_seconds()))
        for violation in violations:
            self.metrics.policy_rejections.labels(violation.code, stage).inc()
        with self.db.transaction():
            self.db.insert("risk_decisions", decision)
            self.audit.append(
                "risk.evaluated",
                correlation_id=cid,
                actor="policy-service",
                payload={
                    "risk_decision_id": decision.id,
                    "proposal_id": proposal.id,
                    "stage": stage,
                    "policy_version": decision.policy_version,
                    "passed": decision.passed,
                    "violations": [v.code for v in violations],
                },
            )
        return decision

    # -- approvals ----------------------------------------------------------
    def decide(
        self, user_id: str, proposal_id: str, proposal_hash: str, decision: Decision
    ) -> tuple[Approval, Order | None]:
        decision = Decision(decision)
        with self.db.lock:
            proposal = self.get_proposal(user_id, proposal_id)
            existing = self.store.approval_for(proposal_id)
            if existing is not None:
                if existing.decision is decision and existing.proposal_hash == proposal_hash:
                    # Replayed request: return the original decision, create nothing.
                    return existing, self.store.order_for_proposal(proposal_id)
                raise Conflict(
                    "ALREADY_DECIDED", f"proposal was already {existing.decision.value}d"
                )
            # Compare against a hash recomputed from stored parameters, not the stored hash.
            if proposal_hash != hash_of(proposal):
                self.audit.append(
                    "approval.hash_mismatch",
                    correlation_id=proposal.correlation_id,
                    actor=user_id,
                    payload={"proposal_id": proposal_id, "presented": proposal_hash},
                )
                raise Conflict(
                    "PROPOSAL_HASH_MISMATCH",
                    "the proposal you reviewed is not the proposal on file; re-fetch and review it",
                )
            if decision is Decision.APPROVE and proposal.status is not ProposalStatus.RISK_PASSED:
                raise Conflict(
                    "RISK_NOT_PASSED",
                    f"proposal is {proposal.status.value}; it must pass risk checks first",
                )
            if decision is Decision.REJECT and proposal.status not in (
                ProposalStatus.DRAFT,
                ProposalStatus.RISK_PASSED,
                ProposalStatus.RISK_REJECTED,
            ):
                raise Conflict("PROPOSAL_NOT_OPEN", f"proposal is {proposal.status.value}")

            now = self.clock.now()
            approval = self.approvals.issue(proposal, user_id, decision, now)
            order = None
            with self.db.transaction():
                self.db.insert("approvals", approval)
                self.store.set_proposal_status(
                    proposal_id,
                    ProposalStatus.APPROVED
                    if decision is Decision.APPROVE
                    else ProposalStatus.REJECTED,
                    now,
                )
                self.audit.append(
                    f"approval.{decision.value}d",
                    correlation_id=proposal.correlation_id,
                    actor=user_id,
                    payload={
                        "approval_id": approval.id,
                        "proposal_id": proposal_id,
                        "proposal_hash": approval.proposal_hash,
                        "expires_at": approval.expires_at.isoformat(),
                    },
                )
                if decision is Decision.APPROVE:
                    order = self.oms.create_order(
                        self.store.proposal(proposal_id), approval, self.new_id("ord")
                    )
            self.metrics.proposal_to_approval.labels(decision.value).observe(
                (now - proposal.created_at).total_seconds()
            )
            return approval, order

    # -- orders -------------------------------------------------------------
    def _own_order(self, user_id: str, order_id: str) -> Order:
        order = self.store.order(order_id)
        if order is None or order.user_id != user_id:
            raise NotFound("ORDER_NOT_FOUND", "no such order")
        return order

    def submit_order(self, user_id: str, order_id: str) -> Order:
        self._own_order(user_id, order_id)
        return self.oms.submit(order_id)

    def get_order(self, user_id: str, order_id: str) -> dict:
        self._own_order(user_id, order_id)
        order = self.oms.refresh(order_id)
        return {"order": order, "fills": self.store.fills(order_id)}

    def cancel_order(self, user_id: str, order_id: str) -> Order:
        self._own_order(user_id, order_id)
        return self.oms.cancel(order_id)

    def list_orders(self, user_id: str) -> list[Order]:
        return self.store.orders_for_user(user_id)

    # -- read-only views ----------------------------------------------------
    def portfolio(self, user_id: str, correlation_id: str | None = None) -> dict:
        principal = Principal(user_id)
        cid = correlation_id or self.new_id("cor")
        try:
            cash = self.tools.call("get_balances", principal, correlation_id=cid)
            positions = self.tools.call("get_positions", principal, correlation_id=cid)
            open_orders = self.tools.call("get_open_orders", principal, correlation_id=cid)
        except BrokerUnavailable:
            raise Unavailable("BROKER_UNAVAILABLE", "paper broker is unreachable") from None
        return {"mode": "paper", "cash": cash, "positions": positions, "open_orders": open_orders}

    def quote(self, user_id: str, symbol: str) -> Quote:
        symbol = symbol.upper()
        if symbol not in self.settings.universe:
            raise NotFound("SYMBOL_NOT_FOUND", f"{symbol} is not in the simulated universe")
        quote = self._quote(user_id, symbol, self.new_id("cor"))
        if quote is None:
            raise Unavailable("MARKET_DATA_UNAVAILABLE", "market data feed is down")
        return quote

    def audit_trail(self, user_id: str, correlation_id: str) -> list[dict]:
        """Events for one correlation ID, visible only to a user who took part in it."""
        events = self.audit.by_correlation(correlation_id)
        owner = self.db.one(
            "SELECT 1 FROM intents WHERE correlation_id = ? AND user_id = ?",
            (correlation_id, user_id),
        )
        if not owner and not any(e["actor"] == user_id for e in events):
            return []
        return events
