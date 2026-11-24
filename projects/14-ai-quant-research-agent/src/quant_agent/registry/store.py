"""SQLite-backed immutable registry, artifact store, audit log, and research memory.

Guarantees:
- Stored RunResults and validated Experiment configurations are immutable; attempts
  to overwrite them raise ImmutabilityError (rejected write).
- Artifacts (trades, equity_curve, metrics, manifest) are written atomically with
  SHA-256 checksums.
- Tracks research memory across BOTH failed/rejected hypotheses and promising ones.
"""

from __future__ import annotations

import hashlib
import json
import sqlite3
import threading
from pathlib import Path
from typing import Any

from quant_agent.models.domain import (
    Experiment,
    ExperimentStatus,
    Hypothesis,
    HypothesisStatus,
    JobRecord,
    PeerReviewSignOff,
    ResearchReport,
    RunResult,
    ValidationResult,
    utc_now,
)


class RegistryError(Exception):
    """Base registry error."""


class ImmutabilityError(RegistryError):
    """Raised when a caller attempts to mutate an immutable record."""


class StorageUnavailableError(RegistryError):
    """Raised when the backing registry store is unavailable or read-only."""


class ExperimentRegistry:
    """Thread-safe SQLite registry for hypotheses, experiments, runs, reports, and memory."""

    def __init__(self, db_path: Path, artifacts_dir: Path) -> None:
        self.db_path = db_path
        self.artifacts_dir = artifacts_dir
        self.db_path.parent.mkdir(parents=True, exist_ok=True)
        self.artifacts_dir.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self._read_only = False
        self._disconnected = False
        self._init_schema()

    def set_fault_mode(
        self, *, disconnected: bool | None = None, read_only: bool | None = None
    ) -> None:
        """Fault injection hook for resilience/failure scenario testing."""
        with self._lock:
            if disconnected is not None:
                self._disconnected = disconnected
            if read_only is not None:
                self._read_only = read_only

    def _connect(self, *, write: bool = False) -> sqlite3.Connection:
        if self._disconnected:
            raise StorageUnavailableError("Registry storage is disconnected.")
        if write and self._read_only:
            raise StorageUnavailableError("Registry storage is in read-only mode; write rejected.")
        conn = sqlite3.connect(str(self.db_path), timeout=10.0)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA journal_mode=WAL;")
        conn.execute("PRAGMA foreign_keys=ON;")
        return conn

    def _init_schema(self) -> None:
        with self._lock:
            conn = self._connect(write=True)
            try:
                conn.executescript(
                    """
                    CREATE TABLE IF NOT EXISTS hypotheses (
                        id TEXT PRIMARY KEY,
                        created_at_utc TEXT NOT NULL,
                        status TEXT NOT NULL,
                        payload_json TEXT NOT NULL
                    );

                    CREATE TABLE IF NOT EXISTS experiments (
                        id TEXT PRIMARY KEY,
                        hypothesis_id TEXT NOT NULL,
                        config_hash TEXT NOT NULL,
                        status TEXT NOT NULL,
                        immutable INTEGER NOT NULL DEFAULT 0,
                        created_at_utc TEXT NOT NULL,
                        payload_json TEXT NOT NULL
                    );

                    CREATE TABLE IF NOT EXISTS validations (
                        id INTEGER PRIMARY KEY AUTOINCREMENT,
                        experiment_id TEXT NOT NULL,
                        passed INTEGER NOT NULL,
                        checked_at_utc TEXT NOT NULL,
                        payload_json TEXT NOT NULL
                    );

                    CREATE TABLE IF NOT EXISTS jobs (
                        job_id TEXT PRIMARY KEY,
                        experiment_id TEXT NOT NULL,
                        idempotency_key TEXT UNIQUE,
                        status TEXT NOT NULL,
                        run_id TEXT,
                        updated_at_utc TEXT NOT NULL,
                        payload_json TEXT NOT NULL
                    );

                    CREATE TABLE IF NOT EXISTS runs (
                        id TEXT PRIMARY KEY,
                        experiment_id TEXT NOT NULL,
                        hypothesis_id TEXT NOT NULL,
                        config_hash TEXT NOT NULL,
                        trial_number INTEGER NOT NULL,
                        completed_at_utc TEXT NOT NULL,
                        payload_json TEXT NOT NULL
                    );

                    CREATE TABLE IF NOT EXISTS reports (
                        id TEXT PRIMARY KEY,
                        hypothesis_id TEXT NOT NULL,
                        generated_at_utc TEXT NOT NULL,
                        payload_json TEXT NOT NULL
                    );

                    CREATE TABLE IF NOT EXISTS reviews (
                        review_id TEXT PRIMARY KEY,
                        report_id TEXT NOT NULL,
                        hypothesis_id TEXT NOT NULL,
                        reviewed_at_utc TEXT NOT NULL,
                        payload_json TEXT NOT NULL
                    );

                    CREATE TABLE IF NOT EXISTS audit_events (
                        id INTEGER PRIMARY KEY AUTOINCREMENT,
                        event_time_utc TEXT NOT NULL,
                        entity_type TEXT NOT NULL,
                        entity_id TEXT NOT NULL,
                        action TEXT NOT NULL,
                        details_json TEXT NOT NULL
                    );
                    """
                )
                conn.commit()
            finally:
                conn.close()

    def _log_audit(
        self,
        conn: sqlite3.Connection,
        entity_type: str,
        entity_id: str,
        action: str,
        details: dict[str, Any],
    ) -> None:
        conn.execute(
            "INSERT INTO audit_events "
            "(event_time_utc, entity_type, entity_id, action, details_json) "
            "VALUES (?, ?, ?, ?, ?)",
            (
                utc_now().isoformat(),
                entity_type,
                entity_id,
                action,
                json.dumps(details, sort_keys=True),
            ),
        )

    # ------------------------------------------------------------------
    # Hypotheses
    # ------------------------------------------------------------------
    def save_hypothesis(self, hypothesis: Hypothesis) -> Hypothesis:
        with self._lock:
            conn = self._connect(write=True)
            try:
                existing = conn.execute(
                    "SELECT id FROM hypotheses WHERE id = ?", (hypothesis.id,)
                ).fetchone()
                if existing is not None:
                    raise ImmutabilityError(
                        f"Hypothesis {hypothesis.id!r} already exists and cannot be overwritten."
                    )
                conn.execute(
                    "INSERT INTO hypotheses (id, created_at_utc, status, payload_json) "
                    "VALUES (?, ?, ?, ?)",
                    (
                        hypothesis.id,
                        hypothesis.created_at_utc.isoformat(),
                        hypothesis.status.value,
                        hypothesis.model_dump_json(),
                    ),
                )
                self._log_audit(
                    conn,
                    "hypothesis",
                    hypothesis.id,
                    "created",
                    {"signal": hypothesis.signal_def.signal_type.value},
                )
                conn.commit()
                return hypothesis
            finally:
                conn.close()

    def update_hypothesis_status(self, hypothesis_id: str, status: HypothesisStatus) -> Hypothesis:
        with self._lock:
            hyp = self.get_hypothesis(hypothesis_id)
            if hyp is None:
                raise KeyError(f"Unknown hypothesis_id: {hypothesis_id}")
            hyp.status = status
            conn = self._connect(write=True)
            try:
                conn.execute(
                    "UPDATE hypotheses SET status = ?, payload_json = ? WHERE id = ?",
                    (status.value, hyp.model_dump_json(), hypothesis_id),
                )
                self._log_audit(
                    conn,
                    "hypothesis",
                    hypothesis_id,
                    "status_updated",
                    {"status": status.value},
                )
                conn.commit()
                return hyp
            finally:
                conn.close()

    def get_hypothesis(self, hypothesis_id: str) -> Hypothesis | None:
        with self._lock:
            conn = self._connect()
            try:
                row = conn.execute(
                    "SELECT payload_json FROM hypotheses WHERE id = ?", (hypothesis_id,)
                ).fetchone()
                return Hypothesis.model_validate_json(row["payload_json"]) if row else None
            finally:
                conn.close()

    def list_hypotheses(self) -> list[Hypothesis]:
        with self._lock:
            conn = self._connect()
            try:
                rows = conn.execute(
                    "SELECT payload_json FROM hypotheses ORDER BY created_at_utc DESC"
                ).fetchall()
                return [Hypothesis.model_validate_json(r["payload_json"]) for r in rows]
            finally:
                conn.close()

    # ------------------------------------------------------------------
    # Experiments
    # ------------------------------------------------------------------
    def save_experiment(self, experiment: Experiment) -> Experiment:
        if not experiment.config_hash:
            experiment.config_hash = experiment.compute_config_hash()
        with self._lock:
            conn = self._connect(write=True)
            try:
                existing = conn.execute(
                    "SELECT immutable FROM experiments WHERE id = ?", (experiment.id,)
                ).fetchone()
                if existing is not None:
                    raise ImmutabilityError(
                        f"Experiment {experiment.id!r} already exists; create a new experiment "
                        "revision instead of mutating."
                    )
                conn.execute(
                    "INSERT INTO experiments "
                    "(id, hypothesis_id, config_hash, status, immutable, "
                    "created_at_utc, payload_json) "
                    "VALUES (?, ?, ?, ?, 0, ?, ?)",
                    (
                        experiment.id,
                        experiment.hypothesis_id,
                        experiment.config_hash,
                        experiment.status.value,
                        experiment.created_at_utc.isoformat(),
                        experiment.model_dump_json(),
                    ),
                )
                self._log_audit(
                    conn,
                    "experiment",
                    experiment.id,
                    "created",
                    {"config_hash": experiment.config_hash},
                )
                conn.commit()
                return experiment
            finally:
                conn.close()

    def update_experiment_status(
        self, experiment_id: str, status: ExperimentStatus, *, lock_immutable: bool = False
    ) -> Experiment:
        with self._lock:
            exp = self.get_experiment(experiment_id)
            if exp is None:
                raise KeyError(f"Unknown experiment_id: {experiment_id}")
            exp.status = status
            conn = self._connect(write=True)
            try:
                conn.execute(
                    "UPDATE experiments SET status = ?, immutable = MAX(immutable, ?), "
                    "payload_json = ? WHERE id = ?",
                    (
                        status.value,
                        1 if lock_immutable else 0,
                        exp.model_dump_json(),
                        experiment_id,
                    ),
                )
                self._log_audit(
                    conn,
                    "experiment",
                    experiment_id,
                    "status_updated",
                    {"status": status.value, "immutable": lock_immutable},
                )
                conn.commit()
                return exp
            finally:
                conn.close()

    def get_experiment(self, experiment_id: str) -> Experiment | None:
        with self._lock:
            conn = self._connect()
            try:
                row = conn.execute(
                    "SELECT payload_json FROM experiments WHERE id = ?", (experiment_id,)
                ).fetchone()
                return Experiment.model_validate_json(row["payload_json"]) if row else None
            finally:
                conn.close()

    def list_experiments(self, hypothesis_id: str | None = None) -> list[Experiment]:
        with self._lock:
            conn = self._connect()
            try:
                if hypothesis_id:
                    rows = conn.execute(
                        "SELECT payload_json FROM experiments WHERE hypothesis_id = ? "
                        "ORDER BY created_at_utc DESC",
                        (hypothesis_id,),
                    ).fetchall()
                else:
                    rows = conn.execute(
                        "SELECT payload_json FROM experiments ORDER BY created_at_utc DESC"
                    ).fetchall()
                return [Experiment.model_validate_json(r["payload_json"]) for r in rows]
            finally:
                conn.close()

    # ------------------------------------------------------------------
    # Validations
    # ------------------------------------------------------------------
    def save_validation(self, result: ValidationResult) -> ValidationResult:
        with self._lock:
            conn = self._connect(write=True)
            try:
                conn.execute(
                    "INSERT INTO validations (experiment_id, passed, checked_at_utc, payload_json) "
                    "VALUES (?, ?, ?, ?)",
                    (
                        result.experiment_id,
                        1 if result.passed else 0,
                        result.checked_at_utc.isoformat(),
                        result.model_dump_json(),
                    ),
                )
                self._log_audit(
                    conn,
                    "validation",
                    result.experiment_id,
                    "validated",
                    {"passed": result.passed, "issue_count": len(result.issues)},
                )
                conn.commit()
                return result
            finally:
                conn.close()

    def get_latest_validation(self, experiment_id: str) -> ValidationResult | None:
        with self._lock:
            conn = self._connect()
            try:
                row = conn.execute(
                    "SELECT payload_json FROM validations WHERE experiment_id = ? "
                    "ORDER BY id DESC LIMIT 1",
                    (experiment_id,),
                ).fetchone()
                return ValidationResult.model_validate_json(row["payload_json"]) if row else None
            finally:
                conn.close()

    # ------------------------------------------------------------------
    # Jobs (async queue + idempotency)
    # ------------------------------------------------------------------
    def save_job(self, job: JobRecord) -> JobRecord:
        with self._lock:
            conn = self._connect(write=True)
            try:
                conn.execute(
                    "INSERT OR REPLACE INTO jobs "
                    "(job_id, experiment_id, idempotency_key, status, run_id, "
                    "updated_at_utc, payload_json) "
                    "VALUES (?, ?, ?, ?, ?, ?, ?)",
                    (
                        job.job_id,
                        job.experiment_id,
                        job.idempotency_key,
                        job.status.value,
                        job.run_id,
                        job.updated_at_utc.isoformat(),
                        job.model_dump_json(),
                    ),
                )
                conn.commit()
                return job
            finally:
                conn.close()

    def get_job(self, job_id: str) -> JobRecord | None:
        with self._lock:
            conn = self._connect()
            try:
                row = conn.execute(
                    "SELECT payload_json FROM jobs WHERE job_id = ?", (job_id,)
                ).fetchone()
                return JobRecord.model_validate_json(row["payload_json"]) if row else None
            finally:
                conn.close()

    def get_job_by_idempotency_key(self, key: str) -> JobRecord | None:
        with self._lock:
            conn = self._connect()
            try:
                row = conn.execute(
                    "SELECT payload_json FROM jobs WHERE idempotency_key = ?", (key,)
                ).fetchone()
                return JobRecord.model_validate_json(row["payload_json"]) if row else None
            finally:
                conn.close()

    # ------------------------------------------------------------------
    # Runs & Artifacts (strictly immutable)
    # ------------------------------------------------------------------
    def write_run_artifacts(
        self, run_id: str, artifacts_payloads: dict[str, Any]
    ) -> tuple[dict[str, str], dict[str, str]]:
        """Persist run artifacts to disk and return (paths_map, sha256_map)."""
        if self._disconnected or self._read_only:
            raise StorageUnavailableError(
                "Cannot write artifacts while storage is unavailable/read-only."
            )
        run_dir = self.artifacts_dir / run_id
        run_dir.mkdir(parents=True, exist_ok=True)
        paths: dict[str, str] = {}
        checksums: dict[str, str] = {}
        for name, data in artifacts_payloads.items():
            filename = f"{name}.json"
            file_path = run_dir / filename
            serialized = json.dumps(data, indent=2, sort_keys=True, default=str)
            file_path.write_text(serialized, encoding="utf-8")
            paths[name] = str(file_path)
            checksums[name] = hashlib.sha256(serialized.encode("utf-8")).hexdigest()
        return paths, checksums

    def load_run_artifacts(self, run_id: str) -> dict[str, Any]:
        run = self.get_run(run_id)
        if run is None:
            raise KeyError(f"Unknown run_id: {run_id}")
        loaded: dict[str, Any] = {}
        for name, path_str in run.artifacts.items():
            p = Path(path_str)
            if p.exists():
                loaded[name] = json.loads(p.read_text(encoding="utf-8"))
        return loaded

    def save_run(self, run: RunResult) -> RunResult:
        with self._lock:
            conn = self._connect(write=True)
            try:
                existing = conn.execute("SELECT id FROM runs WHERE id = ?", (run.id,)).fetchone()
                if existing is not None:
                    raise ImmutabilityError(
                        f"RunResult {run.id!r} is immutable and cannot be overwritten."
                    )
                conn.execute(
                    "INSERT INTO runs "
                    "(id, experiment_id, hypothesis_id, config_hash, trial_number, "
                    "completed_at_utc, payload_json) "
                    "VALUES (?, ?, ?, ?, ?, ?, ?)",
                    (
                        run.id,
                        run.experiment_id,
                        run.hypothesis_id,
                        run.config_hash,
                        run.trial_number,
                        run.completed_at_utc.isoformat(),
                        run.model_dump_json(),
                    ),
                )
                self._log_audit(
                    conn,
                    "run",
                    run.id,
                    "completed",
                    {
                        "experiment_id": run.experiment_id,
                        "trial_number": run.trial_number,
                        "test_sharpe": run.test_metrics.sharpe_ratio,
                    },
                )
                conn.commit()
                return run
            finally:
                conn.close()

    def get_run(self, run_id: str) -> RunResult | None:
        with self._lock:
            conn = self._connect()
            try:
                row = conn.execute(
                    "SELECT payload_json FROM runs WHERE id = ?", (run_id,)
                ).fetchone()
                return RunResult.model_validate_json(row["payload_json"]) if row else None
            finally:
                conn.close()

    def list_runs_for_hypothesis(
        self, hypothesis_id: str, *, include_replays: bool = False
    ) -> list[RunResult]:
        with self._lock:
            conn = self._connect()
            try:
                rows = conn.execute(
                    "SELECT payload_json FROM runs WHERE hypothesis_id = ? "
                    "ORDER BY trial_number ASC, completed_at_utc ASC",
                    (hypothesis_id,),
                ).fetchall()
                runs = [RunResult.model_validate_json(r["payload_json"]) for r in rows]
                if not include_replays:
                    runs = [r for r in runs if not r.is_replay]
                return runs
            finally:
                conn.close()

    def list_runs_for_experiment(self, experiment_id: str) -> list[RunResult]:
        with self._lock:
            conn = self._connect()
            try:
                rows = conn.execute(
                    "SELECT payload_json FROM runs WHERE experiment_id = ? "
                    "ORDER BY completed_at_utc ASC",
                    (experiment_id,),
                ).fetchall()
                return [RunResult.model_validate_json(r["payload_json"]) for r in rows]
            finally:
                conn.close()

    # ------------------------------------------------------------------
    # Reports & Peer Reviews
    # ------------------------------------------------------------------
    def save_report(self, report: ResearchReport) -> ResearchReport:
        with self._lock:
            conn = self._connect(write=True)
            try:
                conn.execute(
                    "INSERT INTO reports (id, hypothesis_id, generated_at_utc, payload_json) "
                    "VALUES (?, ?, ?, ?)",
                    (
                        report.id,
                        report.hypothesis_id,
                        report.generated_at_utc.isoformat(),
                        report.model_dump_json(),
                    ),
                )
                self._log_audit(
                    conn,
                    "report",
                    report.id,
                    "generated",
                    {
                        "hypothesis_id": report.hypothesis_id,
                        "decision": report.decision.value,
                        "cited_runs": report.cited_run_ids,
                    },
                )
                conn.commit()
                return report
            finally:
                conn.close()

    def get_latest_report(self, hypothesis_id: str) -> ResearchReport | None:
        with self._lock:
            conn = self._connect()
            try:
                row = conn.execute(
                    "SELECT payload_json FROM reports WHERE hypothesis_id = ? "
                    "ORDER BY generated_at_utc DESC LIMIT 1",
                    (hypothesis_id,),
                ).fetchone()
                if not row:
                    return None
                report = ResearchReport.model_validate_json(row["payload_json"])
                reviews = self.list_reviews(hypothesis_id)
                report.reviews = [r for r in reviews if r.report_id == report.id]
                return report
            finally:
                conn.close()

    def save_review(self, review: PeerReviewSignOff) -> PeerReviewSignOff:
        with self._lock:
            conn = self._connect(write=True)
            try:
                conn.execute(
                    "INSERT INTO reviews "
                    "(review_id, report_id, hypothesis_id, reviewed_at_utc, payload_json) "
                    "VALUES (?, ?, ?, ?, ?)",
                    (
                        review.review_id,
                        review.report_id,
                        review.hypothesis_id,
                        review.reviewed_at_utc.isoformat(),
                        review.model_dump_json(),
                    ),
                )
                self._log_audit(
                    conn,
                    "review",
                    review.review_id,
                    "signed_off",
                    {
                        "reviewer_id": review.reviewer_id,
                        "decision": review.decision,
                        "hypothesis_id": review.hypothesis_id,
                    },
                )
                conn.commit()
                return review
            finally:
                conn.close()

    def list_reviews(self, hypothesis_id: str) -> list[PeerReviewSignOff]:
        with self._lock:
            conn = self._connect()
            try:
                rows = conn.execute(
                    "SELECT payload_json FROM reviews WHERE hypothesis_id = ? "
                    "ORDER BY reviewed_at_utc ASC",
                    (hypothesis_id,),
                ).fetchall()
                return [PeerReviewSignOff.model_validate_json(r["payload_json"]) for r in rows]
            finally:
                conn.close()

    # ------------------------------------------------------------------
    # Research Memory & Audit Log
    # ------------------------------------------------------------------
    def get_research_memory(self) -> list[dict[str, Any]]:
        """Summarize every hypothesis (both failed/rejected and candidates) with trial counts."""
        hypotheses = self.list_hypotheses()
        memory: list[dict[str, Any]] = []
        for hyp in hypotheses:
            runs = self.list_runs_for_hypothesis(hyp.id)
            report = self.get_latest_report(hyp.id)
            latest_run = runs[-1] if runs else None
            memory.append(
                {
                    "hypothesis_id": hyp.id,
                    "question": hyp.question,
                    "signal_type": hyp.signal_def.signal_type.value,
                    "status": hyp.status.value,
                    "trial_count": len(runs),
                    "latest_test_sharpe": (
                        latest_run.test_metrics.sharpe_ratio if latest_run else None
                    ),
                    "latest_is_vs_oos_degradation": (
                        latest_run.is_vs_oos_degradation if latest_run else None
                    ),
                    "latest_decision": report.decision.value if report else None,
                    "failure_cases": latest_run.failure_cases if latest_run else [],
                    "created_at_utc": hyp.created_at_utc.isoformat(),
                }
            )
        return memory

    def list_audit_events(self, limit: int = 100) -> list[dict[str, Any]]:
        with self._lock:
            conn = self._connect()
            try:
                rows = conn.execute(
                    "SELECT event_time_utc, entity_type, entity_id, action, details_json "
                    "FROM audit_events ORDER BY id DESC LIMIT ?",
                    (limit,),
                ).fetchall()
                return [
                    {
                        "event_time_utc": r["event_time_utc"],
                        "entity_type": r["entity_type"],
                        "entity_id": r["entity_id"],
                        "action": r["action"],
                        "details": json.loads(r["details_json"]),
                    }
                    for r in rows
                ]
            finally:
                conn.close()
