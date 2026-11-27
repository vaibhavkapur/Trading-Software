"""Versioned platform, risk, feed, broker, and execution-policy configuration."""

from __future__ import annotations

import json
from datetime import datetime
from decimal import Decimal
from pathlib import Path
from typing import Any

from pydantic import BaseModel, ConfigDict, Field, model_validator

from trading_platform.domain.errors import LiveModeBlockedError
from trading_platform.domain.events import (
    SCHEMA_VERSION,
    ExecutionMode,
    LiveRolloutBundle,
    Role,
    utc_now,
)


class ExecutionPolicyConfig(BaseModel):
    model_config = ConfigDict(frozen=True)

    policy_id: str = "POL-PAPER-ONLY-DEFAULT-001"
    policy_version: str = "1.0.0"
    execution_mode: ExecutionMode = ExecutionMode.PAPER
    allow_paper_execution: bool = True
    allow_shadow_mode: bool = True
    allow_live_execution: bool = False
    live_rollout_bundle: LiveRolloutBundle | None = None

    @model_validator(mode="after")
    def _validate_policy(self) -> ExecutionPolicyConfig:
        if self.execution_mode == ExecutionMode.LIVE:
            validate_live_enablement(self)
        return self


def validate_live_enablement(
    policy: ExecutionPolicyConfig,
    *,
    now_utc: datetime | None = None,
) -> list[str]:
    """Fail-closed live enablement verification. Raises LiveModeBlockedError if any requirement is missing."""
    now = now_utc or utc_now()
    gaps: list[str] = []

    if not policy.allow_live_execution:
        gaps.append("Policy explicitly disables live execution (allow_live_execution=false)")

    bundle = policy.live_rollout_bundle
    if bundle is None:
        gaps.append("Missing live_rollout_bundle with written approvals and readiness attestations")
    else:
        if not bundle.venue_readiness_verified:
            gaps.append("Venue readiness not verified")
        if not bundle.regulatory_readiness_verified:
            gaps.append("Regulatory readiness not verified")
        if not bundle.independent_risk_review_ref:
            gaps.append("Missing independent risk review reference")
        if not bundle.security_review_ref:
            gaps.append("Missing security review reference")
        if not bundle.soak_report_ref:
            gaps.append("Missing controlled paper soak report reference")

        required_roles = {Role.RISK_OFFICER, Role.COMPLIANCE, Role.CTO}
        valid_roles: set[Role] = set()
        approver_ids: set[str] = set()
        for approval in bundle.approvals:
            if approval.expires_at_utc <= now:
                gaps.append(
                    f"Approval {approval.approval_ref} by {approval.role.value} expired at "
                    f"{approval.expires_at_utc.isoformat()}"
                )
                continue
            if approval.approver_id in approver_ids:
                gaps.append(
                    f"Duplicate approver_id {approval.approver_id} violates separation of duties"
                )
                continue
            approver_ids.add(approval.approver_id)
            valid_roles.add(approval.role)

        missing_roles = sorted(r.value for r in (required_roles - valid_roles))
        if missing_roles:
            gaps.append(f"Missing valid written approvals from roles: {', '.join(missing_roles)}")

    if gaps:
        raise LiveModeBlockedError(
            "Live trading mode is strictly blocked until all written approvals and "
            "venue/regulatory readiness gates are verified.",
            details={"policy_id": policy.policy_id, "gaps": gaps},
        )
    return []


class RiskLimitsConfig(BaseModel):
    model_config = ConfigDict(frozen=True)

    policy_version: str = "1.0.0"
    allowed_symbols: list[str] = Field(default_factory=lambda: ["AAPL", "MSFT", "NVDA", "SPY"])
    max_order_quantity: Decimal = Decimal("500")
    max_order_notional: Decimal = Decimal("100000")
    max_symbol_position_qty: Decimal = Decimal("1000")
    max_symbol_notional: Decimal = Decimal("200000")
    max_gross_notional: Decimal = Decimal("500000")
    max_daily_loss: Decimal = Decimal("15000")
    max_price_collar_bps: Decimal = Decimal("250")  # 2.50% away from reference tick
    max_orders_per_minute_per_strategy: int = Field(default=60, ge=1)
    halt_on_critical_reconciliation_break: bool = True


class FeedConfig(BaseModel):
    model_config = ConfigDict(frozen=True)

    max_staleness_ms: int = Field(default=5000, ge=1)
    max_clock_skew_ms: int = Field(default=2000, ge=1)
    require_entitlement: bool = True


class BrokerConfig(BaseModel):
    model_config = ConfigDict(frozen=True)

    adapter_type: str = "paper_sandbox"
    venue_name: str = "PAPER_SANDBOX"
    commission_per_share: Decimal = Decimal("0.005")
    min_commission: Decimal = Decimal("1.00")
    slippage_bps: Decimal = Decimal("2.0")
    partial_fill_ratio: Decimal = Decimal("1.0")
    deterministic_seed: int = 42


class StrategyConfig(BaseModel):
    model_config = ConfigDict(frozen=True)

    strategy_id: str
    strategy_type: str  # "momentum_crossover" | "mean_reversion"
    strategy_version: str = "1.0.0"
    symbols: list[str]
    enabled: bool = True
    params: dict[str, Any] = Field(default_factory=dict)


class PlatformConfig(BaseModel):
    model_config = ConfigDict(frozen=True)

    schema_version: str = SCHEMA_VERSION
    config_version: str = "1.0.0"
    environment: str = "local_paper"
    account_id: str = "ACCT-PAPER-001"
    base_currency: str = "USD"
    initial_cash: Decimal = Decimal("1000000.00")
    db_path: str = "data/runtime/platform.db"
    execution_policy: ExecutionPolicyConfig = Field(default_factory=ExecutionPolicyConfig)
    risk_limits: RiskLimitsConfig = Field(default_factory=RiskLimitsConfig)
    feed: FeedConfig = Field(default_factory=FeedConfig)
    broker: BrokerConfig = Field(default_factory=BrokerConfig)
    strategies: list[StrategyConfig] = Field(default_factory=list)

    @classmethod
    def from_json_file(cls, path: str | Path) -> PlatformConfig:
        raw = json.loads(Path(path).read_text(encoding="utf-8"))
        return cls.model_validate(raw)
