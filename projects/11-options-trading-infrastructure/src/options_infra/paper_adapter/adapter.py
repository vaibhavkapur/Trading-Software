"""Paper multi-leg order adapter with strict broker/risk boundaries and Decimal accounting."""

from __future__ import annotations

import uuid
from datetime import UTC, datetime
from decimal import ROUND_HALF_UP, Decimal
from typing import Literal

from options_infra.domain.models import (
    ModelWarning,
    MultiLegOrder,
    OptionContract,
    OptionQuote,
    OrderLeg,
    OrderSide,
    OrderStatus,
    WarningSeverity,
)
from options_infra.instruments.repository import (
    InstrumentNotFoundError,
    InstrumentRepository,
    QuoteFeedDisconnectedError,
    QuoteNotFoundError,
)
from options_infra.paper_adapter.broker_rules import (
    BrokerCapabilities,
    validate_broker_constraints,
)
from options_infra.paper_adapter.pre_trade_risk import (
    PreTradeRiskConfig,
    evaluate_pre_trade_risk,
)
from options_infra.paper_adapter.store import PaperOrderStore


def _money(value: Decimal) -> Decimal:
    return value.quantize(Decimal("0.01"), rounding=ROUND_HALF_UP)


class PaperMultiLegAdapter:
    """Simulated multi-leg execution adapter — strictly PAPER_SIMULATED, never real money."""

    def __init__(
        self,
        repository: InstrumentRepository,
        store: PaperOrderStore,
        capabilities: BrokerCapabilities | None = None,
        risk_config: PreTradeRiskConfig | None = None,
    ) -> None:
        self.repository = repository
        self.store = store
        self.capabilities = capabilities or BrokerCapabilities()
        self.risk_config = risk_config or PreTradeRiskConfig()

    def _estimate_spread_risk(
        self,
        legs: list[OrderLeg],
        contracts: dict[str, OptionContract],
        quotes: dict[str, OptionQuote],
        quantity: int,
        pricing_mode: Literal["MID", "NATURAL"],
    ) -> tuple[Decimal, Decimal, Decimal, Decimal | None, dict[str, Decimal]]:
        """Compute exact Decimal net debit/credit, max loss, max gain, and leg fills."""
        leg_prices: dict[str, Decimal] = {}
        net_per_unit = Decimal("0")
        multiplier = next(iter(contracts.values())).multiplier

        for leg in legs:
            q = quotes[leg.contract_id]
            if pricing_mode == "NATURAL":
                px = q.ask if leg.side == OrderSide.BUY else q.bid
            else:
                px = q.mid
            px = px.quantize(Decimal("0.0001"), rounding=ROUND_HALF_UP)
            leg_prices[leg.contract_id] = px
            sign = Decimal(leg.ratio) if leg.side == OrderSide.BUY else Decimal(-leg.ratio)
            net_per_unit += sign * px

        qty_mult = Decimal(quantity * multiplier)
        net_total = _money(net_per_unit * qty_mult)

        # Defined-risk vertical spread exact max loss / max gain calculation when 2 legs match
        max_loss = _money(max(Decimal("0.00"), net_total))
        max_gain: Decimal | None = None

        if len(legs) == 2:
            c0 = contracts[legs[0].contract_id]
            c1 = contracts[legs[1].contract_id]
            if (
                c0.underlying == c1.underlying
                and c0.expiry == c1.expiry
                and c0.right == c1.right
                and legs[0].side != legs[1].side
                and legs[0].ratio == legs[1].ratio
            ):
                width = abs(c0.strike - c1.strike) * Decimal(legs[0].ratio)
                if net_per_unit >= Decimal("0"):
                    # Debit spread
                    max_loss = _money(net_per_unit * qty_mult)
                    max_gain = _money(max(Decimal("0.00"), (width - net_per_unit) * qty_mult))
                else:
                    # Credit spread
                    credit = -net_per_unit
                    max_gain = _money(credit * qty_mult)
                    max_loss = _money(max(Decimal("0.00"), (width - credit) * qty_mult))

        return net_per_unit, net_total, max_loss, max_gain, leg_prices

    def submit_order(
        self,
        legs: list[OrderLeg],
        quantity: int = 1,
        strategy_type: str = "VERTICAL_SPREAD",
        account_id: str = "PAPER-DEFAULT",
        idempotency_key: str | None = None,
        pricing_mode: Literal["MID", "NATURAL"] = "NATURAL",
        limit_net_debit_credit: Decimal | None = None,
        auto_fill: bool = True,
        asof: datetime | None = None,
    ) -> tuple[MultiLegOrder, bool]:
        """Validate, risk-check, persist, and simulate a multi-leg paper order.

        Returns `(order, deduplicated)` where `deduplicated=True` indicates a duplicate
        `idempotency_key` replay returning the originally persisted order without double-filling.
        """
        if idempotency_key:
            existing = self.store.get_order_by_idempotency_key(idempotency_key)
            if existing is not None:
                return existing, True

        now_utc = (asof or datetime.now(UTC)).astimezone(UTC)
        order_id = f"PAPER-{uuid.uuid4().hex[:12].upper()}"
        warnings: list[ModelWarning] = []

        # Resolve contracts and check broker constraints first
        contracts: dict[str, OptionContract] = {}
        missing_errors: list[str] = []
        for leg in legs:
            try:
                contracts[leg.contract_id] = self.repository.get_contract(leg.contract_id)
            except InstrumentNotFoundError as exc:
                missing_errors.append(str(exc))

        if missing_errors:
            rejected = MultiLegOrder(
                id=order_id,
                idempotency_key=idempotency_key,
                account_id=account_id,
                strategy_type=strategy_type,
                legs=legs,
                quantity=quantity,
                net_debit_credit_per_unit=Decimal("0.00"),
                net_debit_credit=Decimal("0.00"),
                max_loss_estimate=Decimal("0.00"),
                max_gain_estimate=None,
                status=OrderStatus.REJECTED,
                rejection_reason="; ".join(missing_errors),
                warnings=warnings,
                rule_version=self.capabilities.rule_version,
                created_at=now_utc,
                updated_at=now_utc,
            )
            self.store.save_order(rejected)
            return rejected, False

        broker_check = validate_broker_constraints(
            legs=legs,
            contracts_by_id=contracts,
            capabilities=self.capabilities,
            asof=now_utc,
        )
        if not broker_check.allowed:
            rejected = MultiLegOrder(
                id=order_id,
                idempotency_key=idempotency_key,
                account_id=account_id,
                strategy_type=strategy_type,
                legs=legs,
                quantity=quantity,
                net_debit_credit_per_unit=Decimal("0.00"),
                net_debit_credit=Decimal("0.00"),
                max_loss_estimate=Decimal("0.00"),
                max_gain_estimate=None,
                status=OrderStatus.REJECTED,
                rejection_reason=f"BROKER_CONSTRAINT_FAILED: {'; '.join(broker_check.reasons)}",
                warnings=warnings,
                rule_version=broker_check.rule_version,
                created_at=now_utc,
                updated_at=now_utc,
            )
            self.store.save_order(rejected)
            return rejected, False

        # Fetch quotes & inspect health (feed disconnect or missing quote -> explicit rejection)
        quotes: dict[str, OptionQuote] = {}
        try:
            for leg in legs:
                quote = self.repository.get_quote(leg.contract_id)
                quotes[leg.contract_id] = quote
                warnings.extend(self.repository.inspect_quote_health(quote, reference_now=now_utc))
        except (QuoteFeedDisconnectedError, QuoteNotFoundError) as exc:
            rejected = MultiLegOrder(
                id=order_id,
                idempotency_key=idempotency_key,
                account_id=account_id,
                strategy_type=strategy_type,
                legs=legs,
                quantity=quantity,
                net_debit_credit_per_unit=Decimal("0.00"),
                net_debit_credit=Decimal("0.00"),
                max_loss_estimate=Decimal("0.00"),
                max_gain_estimate=None,
                status=OrderStatus.REJECTED,
                rejection_reason=f"MARKET_DATA_UNAVAILABLE: {exc}",
                warnings=warnings,
                rule_version=self.risk_config.rule_version,
                created_at=now_utc,
                updated_at=now_utc,
            )
            self.store.save_order(rejected)
            return rejected, False

        net_unit, net_total, max_loss, max_gain, leg_prices = self._estimate_spread_risk(
            legs=legs,
            contracts=contracts,
            quotes=quotes,
            quantity=quantity,
            pricing_mode=pricing_mode,
        )

        # Pre-trade risk gate (separate from execution)
        risk_decision = evaluate_pre_trade_risk(
            quantity=quantity,
            net_debit_credit=net_total,
            max_loss_estimate=max_loss,
            warnings=warnings,
            config=self.risk_config,
        )
        if not risk_decision.approved:
            rejected = MultiLegOrder(
                id=order_id,
                idempotency_key=idempotency_key,
                account_id=account_id,
                strategy_type=strategy_type,
                legs=legs,
                quantity=quantity,
                net_debit_credit_per_unit=net_unit,
                net_debit_credit=net_total,
                max_loss_estimate=max_loss,
                max_gain_estimate=max_gain,
                status=OrderStatus.REJECTED,
                rejection_reason=f"PRE_TRADE_RISK_REJECTED: {'; '.join(risk_decision.reasons)}",
                warnings=warnings,
                rule_version=risk_decision.rule_version,
                created_at=now_utc,
                updated_at=now_utc,
            )
            self.store.save_order(rejected)
            return rejected, False

        # Determine fill vs resting submitted order based on limit price
        can_fill = auto_fill
        if limit_net_debit_credit is not None and net_unit > limit_net_debit_credit:
            can_fill = False
            warnings.append(
                ModelWarning(
                    code="LIMIT_NOT_MARKETABLE",
                    severity=WarningSeverity.INFO,
                    message=(
                        f"Estimated net price {net_unit} exceeds limit {limit_net_debit_credit}; "
                        "order remains resting in SUBMITTED state."
                    ),
                )
            )

        status = OrderStatus.FILLED if can_fill else OrderStatus.SUBMITTED
        fill_px = net_unit if can_fill else None

        order = MultiLegOrder(
            id=order_id,
            idempotency_key=idempotency_key,
            account_id=account_id,
            strategy_type=strategy_type,
            legs=legs,
            quantity=quantity,
            net_debit_credit_per_unit=net_unit,
            net_debit_credit=net_total,
            max_loss_estimate=max_loss,
            max_gain_estimate=max_gain,
            status=status,
            fill_price_per_unit=fill_px,
            warnings=warnings,
            rule_version=f"{broker_check.rule_version}+{risk_decision.rule_version}",
            created_at=now_utc,
            updated_at=now_utc,
        )
        self.store.save_order(order)
        if status == OrderStatus.FILLED:
            self.store.apply_filled_order_to_positions(order, leg_prices)
        return order, False

    def cancel_order(self, order_id: str, asof: datetime | None = None) -> MultiLegOrder:
        """Cancel a resting SUBMITTED paper order; reject invalid state transitions."""
        existing = self.store.get_order(order_id)
        if existing is None:
            raise LookupError(f"Unknown paper order: {order_id}")
        if existing.status not in (OrderStatus.PREVIEWED, OrderStatus.VALIDATED, OrderStatus.SUBMITTED):
            raise ValueError(
                f"Cannot cancel order {order_id} in terminal status {existing.status.value}"
            )
        now_utc = (asof or datetime.now(UTC)).astimezone(UTC)
        cancelled = existing.model_copy(
            update={"status": OrderStatus.CANCELLED, "updated_at": now_utc}
        )
        self.store.save_order(cancelled)
        return cancelled
