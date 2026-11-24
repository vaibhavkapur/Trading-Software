"""API request and response schemas."""

from __future__ import annotations

from datetime import datetime
from decimal import Decimal
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field

from quant_agent.models.domain import (
    DateRange,
    Experiment,
    Hypothesis,
    JobRecord,
    RunResult,
)
from quant_agent.models.signals import (
    SignalDefinition,
    TransactionCostAssumptions,
    WalkForwardConfig,
)


class CreateHypothesisRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    question: str = Field(min_length=5, max_length=1000)
    dataset_id: str | None = None
    universe: list[str] | None = None
    horizon_days: int | None = Field(default=None, ge=1, le=63)
    walk_forward: bool = False
    seed: int = Field(default=20260101, ge=0, le=2**31 - 1)


class CreateHypothesisResponse(BaseModel):
    hypothesis: Hypothesis
    proposed_experiment: Experiment


class CreateExperimentRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    hypothesis_id: str
    dataset_version_id: str
    universe: list[str] = Field(min_length=1)
    signal_def: SignalDefinition
    horizon_days: int = Field(ge=1, le=63)
    train_range: DateRange
    val_range: DateRange
    test_range: DateRange
    purge_days: int = Field(default=5, ge=0, le=60)
    embargo_days: int = Field(default=2, ge=0, le=30)
    initial_capital: Decimal = Field(default=Decimal("1000000.00"), gt=Decimal("0"))
    cost_assumptions: TransactionCostAssumptions = Field(default_factory=TransactionCostAssumptions)
    walk_forward: WalkForwardConfig = Field(default_factory=WalkForwardConfig)
    seed: int = Field(default=20260101, ge=0, le=2**31 - 1)
    entitlement_tier: str = "simulated_internal"
    max_trials_budget: int = Field(default=20, ge=1, le=200)
    asof_timestamp_utc: datetime | None = None


class RunExperimentRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    async_mode: bool = False
    idempotency_key: str | None = None


class RunExperimentResponse(BaseModel):
    job: JobRecord
    run: RunResult | None = None


class ReproduceRunRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    tolerance: float = Field(default=1e-9, gt=0.0, le=1e-2)


class SubmitReviewRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    reviewer_id: str = Field(min_length=2, max_length=100)
    decision: Literal["approved_for_paper_tracking", "changes_requested", "rejected"]
    comments: str = Field(min_length=5, max_length=2000)
