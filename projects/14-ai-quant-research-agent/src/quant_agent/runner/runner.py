"""Queued deterministic backtest runner with idempotency, retries, and replay verification."""

from __future__ import annotations

import time
from concurrent.futures import ThreadPoolExecutor
from decimal import Decimal

from quant_agent.backtest.engine import EventDrivenBacktester
from quant_agent.data.catalog import DatasetCatalog
from quant_agent.metrics.telemetry import ResearchTelemetry
from quant_agent.models.domain import (
    Experiment,
    ExperimentStatus,
    HypothesisStatus,
    JobRecord,
    JobStatus,
    ReproducibilityCheckResult,
    RunResult,
    RunStatus,
    new_id,
    utc_now,
)
from quant_agent.registry.store import ExperimentRegistry
from quant_agent.validator.validator import ExperimentValidator


class ValidationRequiredError(ValueError):
    """Raised when an experiment is submitted to run without passing validation."""


class ExperimentRunner:
    """Executes validated experiments (sync or async queued) and verifies replays."""

    def __init__(
        self,
        *,
        catalog: DatasetCatalog,
        validator: ExperimentValidator,
        backtester: EventDrivenBacktester,
        registry: ExperimentRegistry,
        telemetry: ResearchTelemetry,
        code_revision: str = "0.1.0-local",
        max_workers: int = 2,
    ) -> None:
        self.catalog = catalog
        self.validator = validator
        self.backtester = backtester
        self.registry = registry
        self.telemetry = telemetry
        self.code_revision = code_revision
        self._executor = ThreadPoolExecutor(
            max_workers=max_workers, thread_name_prefix="quant-runner"
        )

    def shutdown(self) -> None:
        self._executor.shutdown(wait=True)

    def submit(
        self,
        experiment_id: str,
        *,
        async_mode: bool = False,
        idempotency_key: str | None = None,
    ) -> tuple[JobRecord, RunResult | None]:
        """Submit a validated experiment for execution (sync or queued async)."""
        if idempotency_key:
            existing = self.registry.get_job_by_idempotency_key(idempotency_key)
            if existing is not None:
                run = self.registry.get_run(existing.run_id) if existing.run_id else None
                return existing, run

        experiment = self.registry.get_experiment(experiment_id)
        if experiment is None:
            raise KeyError(f"Unknown experiment_id: {experiment_id}")

        # Always enforce validation barrier right before execution
        prior_runs = self.registry.list_runs_for_hypothesis(experiment.hypothesis_id)
        validation = self.validator.validate(experiment, existing_trial_count=len(prior_runs))
        self.registry.save_validation(validation)
        if not validation.passed:
            self.registry.update_experiment_status(
                experiment.id, ExperimentStatus.VALIDATION_FAILED
            )
            for issue in validation.issues:
                if issue.severity.value == "error":
                    self.telemetry.record_rejected_experiment(issue.code)
            error_codes = [i.code for i in validation.issues if i.severity.value == "error"]
            raise ValidationRequiredError(
                f"Experiment {experiment.id!r} failed validation: {error_codes}"
            )

        self.registry.update_experiment_status(
            experiment.id, ExperimentStatus.QUEUED, lock_immutable=True
        )

        job = JobRecord(
            experiment_id=experiment.id,
            idempotency_key=idempotency_key,
            status=JobStatus.QUEUED,
        )
        self.registry.save_job(job)

        if async_mode:
            self._executor.submit(self._execute_job, job.job_id, experiment)
            return job, None

        run = self._execute_job(job.job_id, experiment)
        updated_job = self.registry.get_job(job.job_id) or job
        return updated_job, run

    def _execute_job(self, job_id: str, experiment: Experiment) -> RunResult:
        t0 = time.perf_counter()
        job = self.registry.get_job(job_id)
        if job is None:
            raise KeyError(f"Unknown job_id: {job_id}")

        job.status = JobStatus.RUNNING
        job.attempts += 1
        job.updated_at_utc = utc_now()
        self.registry.save_job(job)
        self.registry.update_experiment_status(experiment.id, ExperimentStatus.RUNNING)

        try:
            run = self._run_once(experiment, started_perf=t0)
            job.status = JobStatus.COMPLETED
            job.run_id = run.id
            job.updated_at_utc = utc_now()
            self.registry.save_job(job)
            self.registry.update_experiment_status(
                experiment.id, ExperimentStatus.COMPLETED, lock_immutable=True
            )
            self.registry.update_hypothesis_status(
                experiment.hypothesis_id, HypothesisStatus.TESTED
            )
            return run
        except Exception as exc:
            job.status = JobStatus.FAILED
            job.error_message = str(exc)
            job.updated_at_utc = utc_now()
            self.registry.save_job(job)
            self.registry.update_experiment_status(experiment.id, ExperimentStatus.FAILED)
            raise

    def _run_once(
        self,
        experiment: Experiment,
        *,
        started_perf: float | None = None,
        is_replay: bool = False,
    ) -> RunResult:
        t0 = started_perf if started_perf is not None else time.perf_counter()
        started_at = utc_now()

        prior_runs = self.registry.list_runs_for_hypothesis(experiment.hypothesis_id)
        prior_pvals = [r.test_metrics.p_value_two_sided for r in prior_runs]
        prior_sharpes = [r.test_metrics.sharpe_ratio for r in prior_runs]
        trial_number = len(prior_runs) + 1 if not is_replay else max(1, len(prior_runs))

        ds = self.catalog.get_version(experiment.dataset_version_id)
        assert ds is not None

        output, mt_summary = self.backtester.run(
            experiment,
            prior_trial_p_values=prior_pvals if not is_replay else prior_pvals[:-1],
            prior_trial_sharpes=prior_sharpes if not is_replay else prior_sharpes[:-1],
        )

        run_id = new_id("run")
        artifacts_data = {
            "config": experiment.model_dump(mode="json"),
            "metrics": {
                "train": output.train_metrics.model_dump(mode="json"),
                "val": output.val_metrics.model_dump(mode="json"),
                "test": output.test_metrics.model_dump(mode="json"),
                "walk_forward_folds": [
                    f.model_dump(mode="json") for f in output.walk_forward_folds
                ],
                "multiple_testing": mt_summary.model_dump(mode="json"),
                "is_vs_oos_degradation": output.is_vs_oos_degradation,
            },
            "trades": [t.model_dump(mode="json") for t in output.trades],
            "equity_curve": output.equity_curve,
            "manifest": {
                "run_id": run_id,
                "experiment_id": experiment.id,
                "config_hash": experiment.config_hash or experiment.compute_config_hash(),
                "dataset_version_id": ds.id,
                "dataset_checksum": ds.checksum,
                "code_revision": self.code_revision,
                "validator_version": self.validator.validator_version,
                "seed": experiment.seed,
                "trades_digest": output.trades_digest,
                "paper_simulated_only": True,
            },
        }
        artifact_paths, artifact_checksums = self.registry.write_run_artifacts(
            run_id, artifacts_data
        )

        duration_ms = round((time.perf_counter() - t0) * 1000.0, 3)
        run = RunResult(
            id=run_id,
            experiment_id=experiment.id,
            hypothesis_id=experiment.hypothesis_id,
            config_hash=experiment.config_hash or experiment.compute_config_hash(),
            dataset_version_id=ds.id,
            dataset_checksum=ds.checksum,
            code_revision=self.code_revision,
            validator_version=self.validator.validator_version,
            seed=experiment.seed,
            trial_number=trial_number,
            status=RunStatus.COMPLETED,
            started_at_utc=started_at,
            completed_at_utc=utc_now(),
            duration_ms=duration_ms,
            train_metrics=output.train_metrics,
            val_metrics=output.val_metrics,
            test_metrics=output.test_metrics,
            walk_forward_folds=output.walk_forward_folds,
            is_vs_oos_degradation=output.is_vs_oos_degradation,
            multiple_testing=mt_summary,
            failure_cases=output.failure_cases,
            artifacts=artifact_paths,
            artifact_checksums=artifact_checksums,
            trades_digest=output.trades_digest,
            is_replay=is_replay,
            paper_simulated_only=True,
        )
        self.registry.save_run(run)

        if not is_replay:
            self.telemetry.record_completed_run(
                duration_seconds=duration_ms / 1000.0,
                is_vs_oos_degradation=run.is_vs_oos_degradation,
            )
        return run

    def reproduce_run(
        self, original_run_id: str, *, tolerance: float = 1e-9
    ) -> ReproducibilityCheckResult:
        """Re-execute a pinned experiment and assert trades & metrics match within tolerance."""
        orig = self.registry.get_run(original_run_id)
        if orig is None:
            raise KeyError(f"Unknown run_id: {original_run_id}")
        exp = self.registry.get_experiment(orig.experiment_id)
        if exp is None:
            raise KeyError(f"Unknown experiment_id: {orig.experiment_id}")

        replay = self._run_once(exp, is_replay=True)

        sharpe_diffs = [
            abs(orig.train_metrics.sharpe_ratio - replay.train_metrics.sharpe_ratio),
            abs(orig.val_metrics.sharpe_ratio - replay.val_metrics.sharpe_ratio),
            abs(orig.test_metrics.sharpe_ratio - replay.test_metrics.sharpe_ratio),
        ]
        ret_diffs = [
            abs(orig.train_metrics.total_return - replay.train_metrics.total_return),
            abs(orig.val_metrics.total_return - replay.val_metrics.total_return),
            abs(orig.test_metrics.total_return - replay.test_metrics.total_return),
        ]
        pnl_diff = abs(orig.test_metrics.net_pnl - replay.test_metrics.net_pnl)
        digest_match = orig.trades_digest == replay.trades_digest
        max_sharpe_diff = max(sharpe_diffs)
        max_ret_diff = max(ret_diffs)

        reproducible = (
            digest_match
            and max_sharpe_diff <= tolerance
            and max_ret_diff <= tolerance
            and pnl_diff <= Decimal(str(tolerance))
        )
        self.telemetry.record_reproducibility_check(reproducible)

        return ReproducibilityCheckResult(
            original_run_id=orig.id,
            replay_run_id=replay.id,
            reproducible=reproducible,
            trades_digest_match=digest_match,
            max_sharpe_abs_diff=max_sharpe_diff,
            max_return_abs_diff=max_ret_diff,
            net_pnl_abs_diff=pnl_diff,
            tolerance=tolerance,
        )
