"""Core domain models for the AI Quant Research Agent.

All timestamps are timezone-aware UTC, distinguishing venue/event time from
local receive time. Monetary amounts in execution records use fixed-point
Decimal values.
"""

from __future__ import annotations

import hashlib
import json
from datetime import UTC, date, datetime
from decimal import Decimal
from enum import StrEnum
from typing import Any
from uuid import uuid4

from pydantic import BaseModel, ConfigDict, Field, field_validator

from quant_agent.models.signals import (
    ExecutionPriceMode,
    SignalDefinition,
    TransactionCostAssumptions,
    WalkForwardConfig,
)


def utc_now() -> datetime:
    """Return current timezone-aware UTC timestamp."""
    return datetime.now(UTC)


def ensure_utc(dt: datetime) -> datetime:
    """Ensure a datetime is timezone-aware UTC."""
    if dt.tzinfo is None:
        raise ValueError("Timestamp must be timezone-aware UTC.")
    return dt.astimezone(UTC)


def new_id(prefix: str) -> str:
    return f"{prefix}_{uuid4().hex[:12]}"


class DateRange(BaseModel):
    """Inclusive UTC date range [start, end]."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    start: date
    end: date

    @property
    def calendar_days(self) -> int:
        return (self.end - self.start).days


class HypothesisStatus(StrEnum):
    DRAFT = "draft"
    TESTED = "tested"
    REJECTED = "rejected"
    PROMOTED_FOR_REVIEW = "promoted_for_review"
    APPROVED_PAPER_ONLY = "approved_paper_only"


class Hypothesis(BaseModel):
    """Falsifiable trading hypothesis with explicit null hypothesis."""

    model_config = ConfigDict(extra="forbid")

    id: str = Field(default_factory=lambda: new_id("hyp"))
    question: str = Field(min_length=5, max_length=1000)
    statement: str = Field(min_length=10, max_length=2000)
    null_hypothesis: str = Field(min_length=10, max_length=2000)
    universe: list[str] = Field(min_length=1, max_length=100)
    signal_def: SignalDefinition
    horizon_days: int = Field(ge=1, le=63)
    dataset_id: str
    economic_rationale: str = Field(default="", max_length=2000)
    planner_version: str = "1.0.0"
    created_at_utc: datetime = Field(default_factory=utc_now)
    status: HypothesisStatus = HypothesisStatus.DRAFT

    @field_validator("created_at_utc")
    @classmethod
    def validate_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class DatasetVersion(BaseModel):
    """Allowlisted, checksummed dataset version with point-in-time availability."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    id: str
    name: str
    source: str
    checksum: str
    available_asof: datetime
    coverage_start: date
    coverage_end: date
    symbols: list[str]
    entitlement_tier: str = "simulated_internal"
    is_allowlisted: bool = True
    seed: int = 42
    schema_version: str = "1.0.0"

    @field_validator("available_asof")
    @classmethod
    def validate_asof(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class ExperimentStatus(StrEnum):
    CREATED = "created"
    VALIDATED = "validated"
    VALIDATION_FAILED = "validation_failed"
    QUEUED = "queued"
    RUNNING = "running"
    COMPLETED = "completed"
    FAILED = "failed"


class Experiment(BaseModel):
    """Immutable-once-validated experiment configuration with strict train/val/test splits."""

    model_config = ConfigDict(extra="forbid")

    id: str = Field(default_factory=lambda: new_id("exp"))
    hypothesis_id: str
    dataset_version_id: str
    universe: list[str] = Field(min_length=1)
    signal_def: SignalDefinition
    horizon_days: int = Field(ge=1, le=63)
    train_range: DateRange
    val_range: DateRange
    test_range: DateRange
    purge_days: int = Field(
        default=5,
        ge=0,
        le=60,
        description="Gap days between train->val and val->test to prevent label overlap.",
    )
    embargo_days: int = Field(
        default=2,
        ge=0,
        le=30,
        description="Post-split embargo buffer in calendar days.",
    )
    initial_capital: Decimal = Field(default=Decimal("1000000.00"), gt=Decimal("0"))
    cost_assumptions: TransactionCostAssumptions = Field(default_factory=TransactionCostAssumptions)
    execution_price_mode: ExecutionPriceMode = "next_open"
    walk_forward: WalkForwardConfig = Field(default_factory=WalkForwardConfig)
    seed: int = Field(default=20260101, ge=0, le=2**31 - 1)
    entitlement_tier: str = "simulated_internal"
    max_trials_budget: int = Field(default=20, ge=1, le=200)
    asof_timestamp_utc: datetime = Field(default_factory=utc_now)
    config_hash: str = ""
    status: ExperimentStatus = ExperimentStatus.CREATED
    created_at_utc: datetime = Field(default_factory=utc_now)

    @field_validator("asof_timestamp_utc", "created_at_utc")
    @classmethod
    def validate_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)

    def compute_config_hash(self) -> str:
        """Deterministic SHA-256 hash over execution-relevant parameters."""
        payload = {
            "hypothesis_id": self.hypothesis_id,
            "dataset_version_id": self.dataset_version_id,
            "universe": sorted(self.universe),
            "signal_def": self.signal_def.model_dump(mode="json"),
            "horizon_days": self.horizon_days,
            "train_range": self.train_range.model_dump(mode="json"),
            "val_range": self.val_range.model_dump(mode="json"),
            "test_range": self.test_range.model_dump(mode="json"),
            "purge_days": self.purge_days,
            "embargo_days": self.embargo_days,
            "initial_capital": str(self.initial_capital),
            "cost_assumptions": self.cost_assumptions.model_dump(mode="json"),
            "execution_price_mode": self.execution_price_mode,
            "walk_forward": self.walk_forward.model_dump(mode="json"),
            "seed": self.seed,
        }
        canonical = json.dumps(payload, sort_keys=True, separators=(",", ":"))
        return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


class ValidationIssueSeverity(StrEnum):
    ERROR = "error"
    WARNING = "warning"


class ValidationIssue(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)

    code: str
    severity: ValidationIssueSeverity
    message: str
    field: str = ""


class ValidationResult(BaseModel):
    """Structured output from ExperimentValidator with versioned rule metadata."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    experiment_id: str
    passed: bool
    validator_version: str
    checked_at_utc: datetime = Field(default_factory=utc_now)
    dataset_checksum_verified: bool = False
    issues: list[ValidationIssue] = Field(default_factory=list)
    checks_run: list[str] = Field(default_factory=list)


class MarketBar(BaseModel):
    """OHLCV bar with distinct venue event_time and local receive_time (UTC) and Decimal prices."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    symbol: str
    trading_date: date
    event_time_utc: datetime
    receive_time_utc: datetime
    open: Decimal
    high: Decimal
    low: Decimal
    close: Decimal
    volume: int = Field(ge=0)
    is_stale: bool = False

    @field_validator("event_time_utc", "receive_time_utc")
    @classmethod
    def validate_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class TradeRecord(BaseModel):
    """Executed paper/simulated trade using integer shares and fixed-point Decimal money."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    trade_id: str
    symbol: str
    side: str  # BUY or SELL
    quantity: int = Field(gt=0)
    signal_date: date
    execution_date: date
    event_time_utc: datetime
    receive_time_utc: datetime
    reference_price: Decimal
    fill_price: Decimal
    notional: Decimal
    commission: Decimal
    spread_cost: Decimal
    impact_cost: Decimal
    total_cost: Decimal
    split_name: str  # train | val | test | fold_N


class SplitMetrics(BaseModel):
    """Performance and statistical metrics for a single evaluation split."""

    model_config = ConfigDict(extra="forbid")

    split_name: str
    start_date: date
    end_date: date
    trading_days: int
    num_trades: int
    total_return: float
    annualized_return: float
    annualized_volatility: float
    sharpe_ratio: float
    sortino_ratio: float
    max_drawdown: float
    calmar_ratio: float
    win_rate: float
    profit_factor: float
    turnover_annualized: float
    gross_pnl: Decimal
    total_transaction_costs: Decimal
    net_pnl: Decimal
    t_statistic: float
    p_value_two_sided: float
    bootstrap_sharpe_ci_low: float
    bootstrap_sharpe_ci_high: float
    bootstrap_mean_return_ci_low: float
    bootstrap_mean_return_ci_high: float


class MultipleTestingSummary(BaseModel):
    """Multiple-testing adjustments across all trials for a hypothesis."""

    model_config = ConfigDict(extra="forbid")

    trial_count: int
    raw_p_value: float
    bonferroni_p_value: float
    bh_fdr_adjusted_p_value: float
    deflated_sharpe_probability: float
    warning_triggered: bool
    warning_message: str


class RunStatus(StrEnum):
    COMPLETED = "completed"
    FAILED = "failed"


class RunResult(BaseModel):
    """Immutable stored output of a deterministic backtest run."""

    model_config = ConfigDict(extra="forbid")

    id: str = Field(default_factory=lambda: new_id("run"))
    experiment_id: str
    hypothesis_id: str
    config_hash: str
    dataset_version_id: str
    dataset_checksum: str
    code_revision: str
    validator_version: str
    seed: int
    trial_number: int = Field(ge=1)
    status: RunStatus = RunStatus.COMPLETED
    started_at_utc: datetime = Field(default_factory=utc_now)
    completed_at_utc: datetime = Field(default_factory=utc_now)
    duration_ms: float = 0.0
    train_metrics: SplitMetrics
    val_metrics: SplitMetrics
    test_metrics: SplitMetrics
    walk_forward_folds: list[SplitMetrics] = Field(default_factory=list)
    is_vs_oos_degradation: float = 0.0
    multiple_testing: MultipleTestingSummary
    failure_cases: list[str] = Field(default_factory=list)
    artifacts: dict[str, str] = Field(default_factory=dict)
    artifact_checksums: dict[str, str] = Field(default_factory=dict)
    trades_digest: str = ""
    is_replay: bool = False
    paper_simulated_only: bool = True

    @field_validator("started_at_utc", "completed_at_utc")
    @classmethod
    def validate_utc(cls, v: datetime) -> datetime:
        return ensure_utc(v)


class JobStatus(StrEnum):
    QUEUED = "queued"
    RUNNING = "running"
    COMPLETED = "completed"
    FAILED = "failed"


class JobRecord(BaseModel):
    """Async job tracking record with idempotency support."""

    model_config = ConfigDict(extra="forbid")

    job_id: str = Field(default_factory=lambda: new_id("job"))
    experiment_id: str
    idempotency_key: str | None = None
    status: JobStatus = JobStatus.QUEUED
    run_id: str | None = None
    attempts: int = 0
    error_message: str | None = None
    created_at_utc: datetime = Field(default_factory=utc_now)
    updated_at_utc: datetime = Field(default_factory=utc_now)


class ResearchDecision(StrEnum):
    REJECT_HYPOTHESIS = "reject_hypothesis"
    INCONCLUSIVE_NEEDS_MORE_DATA = "inconclusive_needs_more_data"
    CANDIDATE_FOR_HUMAN_REVIEW = "candidate_for_human_review"
    INSUFFICIENT_EVIDENCE = "insufficient_evidence"


class EvidenceCitation(BaseModel):
    """Links every reported claim/metric directly to a stored RunResult."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    claim: str
    run_id: str
    metric_name: str
    metric_value: str
    split_name: str


class ReproducibilityManifest(BaseModel):
    """Complete manifest required to reproduce trades and metrics bit-for-bit."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    run_id: str
    experiment_id: str
    config_hash: str
    dataset_version_id: str
    dataset_checksum: str
    code_revision: str
    validator_version: str
    seed: int
    trades_digest: str
    artifact_checksums: dict[str, str]


class PeerReviewSignOff(BaseModel):
    """Human peer-review record separated from researcher execution."""

    model_config = ConfigDict(extra="forbid")

    review_id: str = Field(default_factory=lambda: new_id("rev"))
    report_id: str
    hypothesis_id: str
    reviewer_id: str
    decision: str  # approved_for_paper_tracking | changes_requested | rejected
    comments: str
    reviewed_at_utc: datetime = Field(default_factory=utc_now)


class ResearchReport(BaseModel):
    """Evidence-linked research report with strict citations, caveats, and reproducibility."""

    model_config = ConfigDict(extra="forbid")

    id: str = Field(default_factory=lambda: new_id("rpt"))
    hypothesis_id: str
    cited_run_ids: list[str] = Field(min_length=1)
    primary_run_id: str
    trial_count: int = Field(ge=1)
    decision: ResearchDecision
    executive_summary: str
    methodology_summary: str
    performance_table: list[dict[str, Any]]
    citations: list[EvidenceCitation]
    multiple_testing_warnings: list[str]
    failure_cases: list[str]
    caveats: list[str]
    reproducibility_manifest: ReproducibilityManifest
    is_vs_oos_degradation: float
    reporter_version: str = "1.0.0"
    generated_at_utc: datetime = Field(default_factory=utc_now)
    reviews: list[PeerReviewSignOff] = Field(default_factory=list)
    paper_simulated_notice: str = (
        "PAPER/SIMULATED RESEARCH ONLY. No live capital connectivity. "
        "Historical/simulated results do not imply future profitability."
    )


class ReproducibilityCheckResult(BaseModel):
    """Result of re-running a pinned experiment to verify reproducibility."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    original_run_id: str
    replay_run_id: str
    reproducible: bool
    trades_digest_match: bool
    max_sharpe_abs_diff: float
    max_return_abs_diff: float
    net_pnl_abs_diff: Decimal
    tolerance: float
    checked_at_utc: datetime = Field(default_factory=utc_now)
