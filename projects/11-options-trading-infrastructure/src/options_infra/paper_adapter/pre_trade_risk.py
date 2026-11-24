"""Pre-trade risk gate — independently testable boundary before paper order execution."""

from __future__ import annotations

from dataclasses import dataclass
from decimal import Decimal

from options_infra.domain.models import ModelWarning, WarningSeverity


@dataclass(frozen=True)
class PreTradeRiskConfig:
    rule_version: str = "pretrade-risk-1.0.0"
    max_order_quantity: int = 100
    max_loss_per_order: Decimal = Decimal("25000.00")
    max_net_debit_per_order: Decimal = Decimal("25000.00")
    block_on_stale_quote: bool = True
    block_on_clock_skew: bool = True
    kill_switch_active: bool = False


@dataclass(frozen=True)
class PreTradeRiskDecision:
    approved: bool
    reasons: tuple[str, ...]
    rule_version: str


def evaluate_pre_trade_risk(
    quantity: int,
    net_debit_credit: Decimal,
    max_loss_estimate: Decimal,
    warnings: list[ModelWarning],
    config: PreTradeRiskConfig,
) -> PreTradeRiskDecision:
    """Evaluate order risk limits and operational data-quality guards."""
    reasons: list[str] = []

    if config.kill_switch_active:
        reasons.append("Paper execution kill switch is active")

    if quantity <= 0:
        reasons.append(f"Quantity must be positive, got {quantity}")
    elif quantity > config.max_order_quantity:
        reasons.append(
            f"Order quantity {quantity} exceeds pre-trade limit {config.max_order_quantity}"
        )

    if max_loss_estimate > config.max_loss_per_order:
        reasons.append(
            f"Max loss estimate {max_loss_estimate} exceeds limit {config.max_loss_per_order}"
        )

    if net_debit_credit > config.max_net_debit_per_order:
        reasons.append(
            f"Net debit {net_debit_credit} exceeds limit {config.max_net_debit_per_order}"
        )

    for w in warnings:
        if config.block_on_stale_quote and w.code == "STALE_QUOTE":
            reasons.append(f"Blocked by data-freshness guard: {w.message}")
        if config.block_on_clock_skew and w.code == "CLOCK_SKEW_DETECTED":
            reasons.append(f"Blocked by clock-skew guard: {w.message}")
        if w.severity == WarningSeverity.CRITICAL and w.code in {
            "DEBIT_EXCEEDS_SPREAD_WIDTH",
            "CREDIT_SPREAD_HAS_NET_DEBIT",
        }:
            reasons.append(f"Blocked by pricing sanity guard ({w.code}): {w.message}")

    return PreTradeRiskDecision(
        approved=len(reasons) == 0,
        reasons=tuple(reasons),
        rule_version=config.rule_version,
    )
