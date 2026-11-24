"""Broker capability and multi-leg constraint checks (separated from execution)."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import UTC, datetime
from decimal import Decimal

from options_infra.domain.models import (
    OptionContract,
    OrderLeg,
    OrderSide,
)


@dataclass(frozen=True)
class BrokerCapabilities:
    """Explicit simulated broker multi-leg order support profile."""

    broker_name: str = "PAPER_SIM_BROKER"
    rule_version: str = "broker-rules-1.0.0"
    supports_multi_leg: bool = True
    max_legs: int = 4
    allow_mixed_underlyings: bool = False
    allow_mixed_expiries: bool = False
    require_defined_risk: bool = True
    max_ratio_per_leg: int = 10


@dataclass(frozen=True)
class BrokerConstraintResult:
    allowed: bool
    reasons: tuple[str, ...]
    rule_version: str


def validate_broker_constraints(
    legs: list[OrderLeg],
    contracts_by_id: dict[str, OptionContract],
    capabilities: BrokerCapabilities,
    asof: datetime | None = None,
) -> BrokerConstraintResult:
    """Verify broker support and leg-level constraints before order routing."""
    reasons: list[str] = []
    now_date = (asof or datetime.now(UTC)).astimezone(UTC).date()

    if not legs:
        return BrokerConstraintResult(
            allowed=False,
            reasons=("Order must contain at least one leg",),
            rule_version=capabilities.rule_version,
        )

    if len(legs) > 1 and not capabilities.supports_multi_leg:
        reasons.append(f"Broker {capabilities.broker_name} does not support multi-leg orders")

    if len(legs) > capabilities.max_legs:
        reasons.append(
            f"Order has {len(legs)} legs, exceeding broker limit of {capabilities.max_legs}"
        )

    seen_contracts: set[str] = set()
    underlyings: set[str] = set()
    expiries: set[str] = set()
    resolved_contracts: list[tuple[OrderLeg, OptionContract]] = []

    for leg in legs:
        if leg.contract_id in seen_contracts:
            reasons.append(f"Duplicate leg referencing contract {leg.contract_id}")
            continue
        seen_contracts.add(leg.contract_id)

        contract = contracts_by_id.get(leg.contract_id)
        if contract is None:
            reasons.append(f"Unknown contract {leg.contract_id}")
            continue

        if not contract.active:
            reasons.append(f"Contract {leg.contract_id} is inactive/halted")

        if contract.expiry < now_date:
            reasons.append(
                f"Contract {leg.contract_id} expired on {contract.expiry.isoformat()}"
            )

        if leg.ratio > capabilities.max_ratio_per_leg:
            reasons.append(
                f"Leg ratio {leg.ratio} for {leg.contract_id} exceeds max "
                f"{capabilities.max_ratio_per_leg}"
            )

        if leg.limit_price is not None:
            rem = leg.limit_price % contract.tick_size
            if rem != Decimal("0"):
                reasons.append(
                    f"Limit price {leg.limit_price} for {leg.contract_id} is not aligned to "
                    f"tick size {contract.tick_size}"
                )

        underlyings.add(contract.underlying)
        expiries.add(contract.expiry.isoformat())
        resolved_contracts.append((leg, contract))

    if len(underlyings) > 1 and not capabilities.allow_mixed_underlyings:
        reasons.append(
            f"Mixed underlyings {sorted(underlyings)} rejected by broker combo rules"
        )

    if len(expiries) > 1 and not capabilities.allow_mixed_expiries:
        reasons.append(
            f"Mixed expiries {sorted(expiries)} rejected by broker vertical-combo rules"
        )

    # Defined-risk check: short options must be covered by at least equal ratio of long options
    # of the same right on the same underlying/expiry.
    if capabilities.require_defined_risk and resolved_contracts:
        net_by_right: dict[str, int] = {"CALL": 0, "PUT": 0}
        for leg, contract in resolved_contracts:
            signed_ratio = leg.ratio if leg.side == OrderSide.BUY else -leg.ratio
            net_by_right[contract.right.value] += signed_ratio
        for right, net_ratio in net_by_right.items():
            if net_ratio < 0:
                reasons.append(
                    f"Uncovered short {right} exposure (net ratio {net_ratio}) rejected: "
                    "broker profile requires defined-risk structures"
                )

    return BrokerConstraintResult(
        allowed=len(reasons) == 0,
        reasons=tuple(reasons),
        rule_version=capabilities.rule_version,
    )
