"""FastAPI application for the AI Quant Research Agent.

Implements the documented research orchestration API with clear separation
between:
- authentication / role authorization (researcher vs reviewer)
- constrained planning
- pre-execution leakage validation
- deterministic paper backtest execution
- immutable registry + artifact retrieval
- evidence-linked reporting & human peer-review sign-off
"""

from __future__ import annotations

from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any

from fastapi import Depends, FastAPI, Header, HTTPException, Response, status
from fastapi.responses import HTMLResponse

from quant_agent.api.schemas import (
    CreateExperimentRequest,
    CreateHypothesisRequest,
    CreateHypothesisResponse,
    ReproduceRunRequest,
    RunExperimentRequest,
    RunExperimentResponse,
    SubmitReviewRequest,
)
from quant_agent.backtest.engine import EventDrivenBacktester
from quant_agent.config import Settings
from quant_agent.data.catalog import DatasetCatalog
from quant_agent.metrics.telemetry import ResearchTelemetry
from quant_agent.models.domain import (
    DatasetVersion,
    Experiment,
    ExperimentStatus,
    Hypothesis,
    HypothesisStatus,
    JobRecord,
    PeerReviewSignOff,
    ReproducibilityCheckResult,
    ResearchReport,
    RunResult,
    ValidationResult,
    utc_now,
)
from quant_agent.planner.planner import ResearchPlanner
from quant_agent.registry.store import (
    ExperimentRegistry,
    ImmutabilityError,
    StorageUnavailableError,
)
from quant_agent.reporter.reporter import ResearchReporter
from quant_agent.runner.runner import ExperimentRunner, ValidationRequiredError
from quant_agent.validator.validator import ExperimentValidator


class ResearchApplicationState:
    """Explicitly wired service container (independently testable components)."""

    def __init__(self, settings: Settings | None = None) -> None:
        self.settings = settings or Settings()
        self.catalog = DatasetCatalog()
        self.telemetry = ResearchTelemetry()
        self.registry = ExperimentRegistry(
            db_path=self.settings.db_path,
            artifacts_dir=self.settings.artifacts_dir,
        )
        self.planner = ResearchPlanner(self.catalog, planner_version=self.settings.planner_version)
        self.validator = ExperimentValidator(
            self.catalog,
            validator_version=self.settings.validator_version,
            max_clock_skew_seconds=self.settings.max_clock_skew_seconds,
        )
        self.backtester = EventDrivenBacktester(self.catalog)
        self.runner = ExperimentRunner(
            catalog=self.catalog,
            validator=self.validator,
            backtester=self.backtester,
            registry=self.registry,
            telemetry=self.telemetry,
            code_revision=self.settings.code_revision,
            max_workers=self.settings.max_workers,
        )
        self.reporter = ResearchReporter(
            self.registry, reporter_version=self.settings.reporter_version
        )

    def close(self) -> None:
        self.runner.shutdown()


def create_app(settings: Settings | None = None) -> FastAPI:
    state = ResearchApplicationState(settings=settings)

    @asynccontextmanager
    async def lifespan(_app: FastAPI):
        try:
            yield
        finally:
            state.close()

    app = FastAPI(
        title="AI Quant Research Agent (Paper/Simulated)",
        version="0.1.0",
        description=(
            "Experiment-orchestrating quant research assistant with strict leakage "
            "validation, deterministic event-driven backtesting, and evidence-linked reports."
        ),
        lifespan=lifespan,
    )
    app.state.research = state

    def require_researcher(
        x_api_token: str | None = Header(default=None),
    ) -> str:
        if state.settings.require_auth and x_api_token != state.settings.researcher_token:
            raise HTTPException(
                status_code=status.HTTP_401_UNAUTHORIZED,
                detail="Researcher authentication required.",
            )
        return "researcher"

    def require_reviewer(
        x_api_token: str | None = Header(default=None),
        x_role: str | None = Header(default=None),
    ) -> str:
        if state.settings.require_auth:
            if x_api_token != state.settings.reviewer_token:
                raise HTTPException(
                    status_code=status.HTTP_403_FORBIDDEN,
                    detail="Reviewer authorization token required for peer sign-off.",
                )
        elif x_role is not None and x_role.lower() != "reviewer":
            raise HTTPException(
                status_code=status.HTTP_403_FORBIDDEN,
                detail="Peer review sign-off requires X-Role: reviewer.",
            )
        return "reviewer"

    # ------------------------------------------------------------------
    # UI, Health & Telemetry
    # ------------------------------------------------------------------
    @app.get("/", response_class=HTMLResponse, include_in_schema=False)
    def workbench_ui() -> HTMLResponse:
        html_path = Path(__file__).resolve().parent.parent / "web" / "index.html"
        return HTMLResponse(content=html_path.read_text(encoding="utf-8"))

    @app.get("/health")
    def health() -> dict[str, Any]:
        return {
            "status": "ok",
            "mode": state.settings.mode,
            "paper_simulated_only": True,
            "validator_version": state.settings.validator_version,
            "timestamp_utc": utc_now().isoformat(),
        }

    @app.get("/v1/metrics")
    def json_metrics() -> dict[str, Any]:
        return state.telemetry.snapshot()

    @app.get("/metrics", include_in_schema=False)
    def prometheus_metrics() -> Response:
        return Response(
            content=state.telemetry.prometheus_bytes(),
            media_type="text/plain; version=0.0.4; charset=utf-8",
        )

    # ------------------------------------------------------------------
    # Datasets & Research Memory
    # ------------------------------------------------------------------
    @app.get("/v1/research/datasets", response_model=list[DatasetVersion])
    def list_datasets() -> list[DatasetVersion]:
        return state.catalog.list_datasets()

    @app.get("/v1/research/memory")
    def research_memory() -> dict[str, Any]:
        try:
            items = state.registry.get_research_memory()
            return {"count": len(items), "hypotheses": items}
        except StorageUnavailableError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc

    # ------------------------------------------------------------------
    # Hypotheses
    # ------------------------------------------------------------------
    @app.post(
        "/v1/research/hypotheses",
        response_model=CreateHypothesisResponse,
        status_code=status.HTTP_201_CREATED,
    )
    def create_hypothesis(
        req: CreateHypothesisRequest,
        _role: str = Depends(require_researcher),
    ) -> CreateHypothesisResponse:
        try:
            hypothesis, experiment = state.planner.propose(
                req.question,
                dataset_id=req.dataset_id,
                universe=req.universe,
                horizon_days=req.horizon_days,
                walk_forward=req.walk_forward,
                seed=req.seed,
            )
            state.registry.save_hypothesis(hypothesis)
            state.registry.save_experiment(experiment)
            return CreateHypothesisResponse(
                hypothesis=hypothesis,
                proposed_experiment=experiment,
            )
        except StorageUnavailableError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc
        except ImmutabilityError as exc:
            raise HTTPException(status_code=409, detail=str(exc)) from exc

    @app.get("/v1/research/hypotheses", response_model=list[Hypothesis])
    def list_hypotheses() -> list[Hypothesis]:
        try:
            return state.registry.list_hypotheses()
        except StorageUnavailableError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc

    @app.get("/v1/research/hypotheses/{hypothesis_id}", response_model=Hypothesis)
    def get_hypothesis(hypothesis_id: str) -> Hypothesis:
        try:
            hyp = state.registry.get_hypothesis(hypothesis_id)
        except StorageUnavailableError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc
        if hyp is None:
            raise HTTPException(status_code=404, detail=f"Hypothesis {hypothesis_id!r} not found.")
        return hyp

    # ------------------------------------------------------------------
    # Experiments: Create, Get, Validate, Run
    # ------------------------------------------------------------------
    @app.post(
        "/v1/research/experiments",
        response_model=Experiment,
        status_code=status.HTTP_201_CREATED,
    )
    def create_experiment(
        req: CreateExperimentRequest,
        _role: str = Depends(require_researcher),
    ) -> Experiment:
        try:
            if state.registry.get_hypothesis(req.hypothesis_id) is None:
                raise HTTPException(
                    status_code=404,
                    detail=f"Hypothesis {req.hypothesis_id!r} not found.",
                )
            exp = Experiment(
                hypothesis_id=req.hypothesis_id,
                dataset_version_id=req.dataset_version_id,
                universe=req.universe,
                signal_def=req.signal_def,
                horizon_days=req.horizon_days,
                train_range=req.train_range,
                val_range=req.val_range,
                test_range=req.test_range,
                purge_days=req.purge_days,
                embargo_days=req.embargo_days,
                initial_capital=req.initial_capital,
                cost_assumptions=req.cost_assumptions,
                walk_forward=req.walk_forward,
                seed=req.seed,
                entitlement_tier=req.entitlement_tier,
                max_trials_budget=req.max_trials_budget,
                asof_timestamp_utc=req.asof_timestamp_utc or utc_now(),
            )
            exp.config_hash = exp.compute_config_hash()
            return state.registry.save_experiment(exp)
        except StorageUnavailableError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc
        except ImmutabilityError as exc:
            raise HTTPException(status_code=409, detail=str(exc)) from exc

    @app.get("/v1/research/experiments/{experiment_id}", response_model=Experiment)
    def get_experiment(experiment_id: str) -> Experiment:
        try:
            exp = state.registry.get_experiment(experiment_id)
        except StorageUnavailableError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc
        if exp is None:
            raise HTTPException(status_code=404, detail=f"Experiment {experiment_id!r} not found.")
        return exp

    @app.post(
        "/v1/research/experiments/{experiment_id}/validate",
        response_model=ValidationResult,
    )
    def validate_experiment(
        experiment_id: str,
        _role: str = Depends(require_researcher),
    ) -> ValidationResult:
        try:
            exp = state.registry.get_experiment(experiment_id)
            if exp is None:
                raise HTTPException(
                    status_code=404, detail=f"Experiment {experiment_id!r} not found."
                )
            prior_runs = state.registry.list_runs_for_hypothesis(exp.hypothesis_id)
            result = state.validator.validate(exp, existing_trial_count=len(prior_runs))
            state.registry.save_validation(result)
            if result.passed:
                state.registry.update_experiment_status(
                    exp.id, ExperimentStatus.VALIDATED, lock_immutable=True
                )
            else:
                state.registry.update_experiment_status(exp.id, ExperimentStatus.VALIDATION_FAILED)
                for issue in result.issues:
                    if issue.severity.value == "error":
                        state.telemetry.record_rejected_experiment(issue.code)
            return result
        except StorageUnavailableError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc

    @app.post(
        "/v1/research/experiments/{experiment_id}/run",
        response_model=RunExperimentResponse,
    )
    def run_experiment(
        experiment_id: str,
        req: RunExperimentRequest | None = None,
        _role: str = Depends(require_researcher),
    ) -> RunExperimentResponse:
        body = req or RunExperimentRequest()
        try:
            job, run = state.runner.submit(
                experiment_id,
                async_mode=body.async_mode,
                idempotency_key=body.idempotency_key,
            )
            return RunExperimentResponse(job=job, run=run)
        except KeyError as exc:
            raise HTTPException(status_code=404, detail=str(exc)) from exc
        except ValidationRequiredError as exc:
            raise HTTPException(status_code=422, detail=str(exc)) from exc
        except StorageUnavailableError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc

    # ------------------------------------------------------------------
    # Jobs, Runs, Artifacts & Reproducibility Verification
    # ------------------------------------------------------------------
    @app.get("/v1/research/jobs/{job_id}", response_model=JobRecord)
    def get_job(job_id: str) -> JobRecord:
        try:
            job = state.registry.get_job(job_id)
        except StorageUnavailableError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc
        if job is None:
            raise HTTPException(status_code=404, detail=f"Job {job_id!r} not found.")
        return job

    @app.get("/v1/research/runs/{run_id}", response_model=RunResult)
    def get_run(run_id: str) -> RunResult:
        try:
            run = state.registry.get_run(run_id)
        except StorageUnavailableError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc
        if run is None:
            raise HTTPException(status_code=404, detail=f"Run {run_id!r} not found.")
        return run

    @app.get("/v1/research/runs/{run_id}/artifacts")
    def get_run_artifacts(run_id: str) -> dict[str, Any]:
        try:
            run = state.registry.get_run(run_id)
            if run is None:
                raise HTTPException(status_code=404, detail=f"Run {run_id!r} not found.")
            payloads = state.registry.load_run_artifacts(run_id)
            return {
                "run_id": run.id,
                "experiment_id": run.experiment_id,
                "config_hash": run.config_hash,
                "trades_digest": run.trades_digest,
                "artifact_checksums": run.artifact_checksums,
                "artifacts": payloads,
            }
        except StorageUnavailableError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc

    @app.post(
        "/v1/research/runs/{run_id}/reproduce",
        response_model=ReproducibilityCheckResult,
    )
    def reproduce_run(
        run_id: str,
        req: ReproduceRunRequest | None = None,
        _role: str = Depends(require_researcher),
    ) -> ReproducibilityCheckResult:
        body = req or ReproduceRunRequest()
        try:
            return state.runner.reproduce_run(run_id, tolerance=body.tolerance)
        except KeyError as exc:
            raise HTTPException(status_code=404, detail=str(exc)) from exc
        except StorageUnavailableError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc

    # ------------------------------------------------------------------
    # Evidence-linked Reports & Human Peer Review
    # ------------------------------------------------------------------
    @app.post(
        "/v1/research/reports/{hypothesis_id}/generate",
        response_model=ResearchReport,
    )
    def generate_report(
        hypothesis_id: str,
        _role: str = Depends(require_researcher),
    ) -> ResearchReport:
        try:
            return state.reporter.generate_report(hypothesis_id)
        except KeyError as exc:
            raise HTTPException(status_code=404, detail=str(exc)) from exc
        except ValueError as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc
        except StorageUnavailableError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc

    @app.get("/v1/research/reports/{hypothesis_id}", response_model=ResearchReport)
    def get_report(hypothesis_id: str) -> ResearchReport:
        try:
            report = state.registry.get_latest_report(hypothesis_id)
            if report is None:
                # Auto-generate only if stored run evidence already exists
                runs = state.registry.list_runs_for_hypothesis(hypothesis_id)
                if not runs:
                    raise HTTPException(
                        status_code=404,
                        detail=(
                            f"No research report or completed runs for hypothesis "
                            f"{hypothesis_id!r}."
                        ),
                    )
                report = state.reporter.generate_report(hypothesis_id)
            return report
        except KeyError as exc:
            raise HTTPException(status_code=404, detail=str(exc)) from exc
        except StorageUnavailableError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc

    @app.post(
        "/v1/research/reports/{hypothesis_id}/review",
        response_model=PeerReviewSignOff,
    )
    def submit_peer_review(
        hypothesis_id: str,
        req: SubmitReviewRequest,
        _role: str = Depends(require_reviewer),
    ) -> PeerReviewSignOff:
        try:
            report = state.registry.get_latest_report(hypothesis_id)
            if report is None:
                raise HTTPException(
                    status_code=404,
                    detail=f"No report exists for hypothesis {hypothesis_id!r} to review.",
                )
            review = PeerReviewSignOff(
                report_id=report.id,
                hypothesis_id=hypothesis_id,
                reviewer_id=req.reviewer_id,
                decision=req.decision,
                comments=req.comments,
            )
            state.registry.save_review(review)
            if req.decision == "approved_for_paper_tracking":
                state.registry.update_hypothesis_status(
                    hypothesis_id, HypothesisStatus.APPROVED_PAPER_ONLY
                )
            elif req.decision == "rejected":
                state.registry.update_hypothesis_status(hypothesis_id, HypothesisStatus.REJECTED)
            else:
                state.registry.update_hypothesis_status(
                    hypothesis_id, HypothesisStatus.PROMOTED_FOR_REVIEW
                )
            return review
        except StorageUnavailableError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc

    return app


app = create_app()
