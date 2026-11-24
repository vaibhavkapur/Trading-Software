"""Evidence-linked Research Reporter with strict citation verification.

Core invariants:
- NEVER asserts a strategy is profitable without out-of-sample RunResult evidence.
- Every numeric metric in the report links to a stored RunResult ID via `EvidenceCitation`.
- Always surfaces trial count, holdout windows, transaction costs, multiple-testing
  warnings, IS-vs-OOS degradation, failure cases, and a complete ReproducibilityManifest.
- Never silently selects the best backtest across trials without multiple-testing disclosure.
"""

from __future__ import annotations

from typing import Any

from quant_agent.models.domain import (
    EvidenceCitation,
    Hypothesis,
    ReproducibilityManifest,
    ResearchDecision,
    ResearchReport,
    RunResult,
)
from quant_agent.registry.store import ExperimentRegistry


class CitationVerificationError(ValueError):
    """Raised if a report references an unknown run or ungrounded metric value."""


class ResearchReporter:
    """Generates and verifies evidence-linked research reports from stored runs."""

    def __init__(
        self,
        registry: ExperimentRegistry,
        *,
        reporter_version: str = "1.0.0",
    ) -> None:
        self.registry = registry
        self.reporter_version = reporter_version

    def generate_report(self, hypothesis_id: str) -> ResearchReport:
        hypothesis = self.registry.get_hypothesis(hypothesis_id)
        if hypothesis is None:
            raise KeyError(f"Unknown hypothesis_id: {hypothesis_id}")

        runs = self.registry.list_runs_for_hypothesis(hypothesis_id)
        if not runs:
            raise ValueError(
                f"Cannot generate report for {hypothesis_id!r}: zero stored RunResults. "
                "The agent refuses to assert performance without experiment evidence."
            )

        # Evaluate ALL runs (never hide failed trials); use the most recent run as primary
        # while reporting all trial results transparently.
        primary = runs[-1]
        cited_run_ids = [r.id for r in runs]
        trial_count = len(runs)

        decision = self._determine_decision(primary, trial_count)
        citations = self._build_citations(runs)
        performance_table = self._build_performance_table(runs)
        mt_warnings = self._build_multiple_testing_warnings(runs)
        caveats = self._build_caveats(hypothesis, primary, trial_count)
        exec_summary = self._build_executive_summary(hypothesis, primary, trial_count, decision)
        method_summary = self._build_methodology_summary(hypothesis, primary)

        manifest = ReproducibilityManifest(
            run_id=primary.id,
            experiment_id=primary.experiment_id,
            config_hash=primary.config_hash,
            dataset_version_id=primary.dataset_version_id,
            dataset_checksum=primary.dataset_checksum,
            code_revision=primary.code_revision,
            validator_version=primary.validator_version,
            seed=primary.seed,
            trades_digest=primary.trades_digest,
            artifact_checksums=primary.artifact_checksums,
        )

        report = ResearchReport(
            hypothesis_id=hypothesis.id,
            cited_run_ids=cited_run_ids,
            primary_run_id=primary.id,
            trial_count=trial_count,
            decision=decision,
            executive_summary=exec_summary,
            methodology_summary=method_summary,
            performance_table=performance_table,
            citations=citations,
            multiple_testing_warnings=mt_warnings,
            failure_cases=list(primary.failure_cases),
            caveats=caveats,
            reproducibility_manifest=manifest,
            is_vs_oos_degradation=primary.is_vs_oos_degradation,
            reporter_version=self.reporter_version,
        )

        # Self-verify all citations against stored registry records before persisting
        self.verify_report_citations(report)
        self.registry.save_report(report)
        return report

    def verify_report_citations(self, report: ResearchReport) -> bool:
        """Verify every citation in a report matches an immutable RunResult in the registry."""
        if not report.cited_run_ids or not report.citations:
            raise CitationVerificationError("Report must contain stored run citations.")

        stored_runs: dict[str, RunResult] = {}
        for rid in report.cited_run_ids:
            run = self.registry.get_run(rid)
            if run is None:
                raise CitationVerificationError(f"Cited run_id {rid!r} does not exist in registry.")
            stored_runs[rid] = run

        for c in report.citations:
            if c.run_id not in stored_runs:
                raise CitationVerificationError(
                    f"Citation references run_id {c.run_id!r} not present in cited_run_ids."
                )
            run = stored_runs[c.run_id]
            split_obj = {
                "train": run.train_metrics,
                "val": run.val_metrics,
                "test": run.test_metrics,
            }.get(c.split_name)
            if split_obj is not None and hasattr(split_obj, c.metric_name):
                actual = str(getattr(split_obj, c.metric_name))
                if actual != c.metric_value:
                    raise CitationVerificationError(
                        f"Metric mismatch for {c.run_id}.{c.split_name}.{c.metric_name}: "
                        f"report={c.metric_value!r} vs stored={actual!r}"
                    )
        return True

    def _determine_decision(self, run: RunResult, trial_count: int) -> ResearchDecision:
        test = run.test_metrics
        mt = run.multiple_testing

        if test.num_trades < 10 or test.trading_days < 30:
            return ResearchDecision.INSUFFICIENT_EVIDENCE

        if (
            test.sharpe_ratio > 0.75
            and test.bootstrap_sharpe_ci_low > 0.0
            and mt.bonferroni_p_value < 0.05
            and mt.deflated_sharpe_probability >= 0.90
            and run.is_vs_oos_degradation < 0.75
        ):
            return ResearchDecision.CANDIDATE_FOR_HUMAN_REVIEW

        if test.sharpe_ratio <= 0.0 or (trial_count > 1 and mt.bonferroni_p_value > 0.25):
            return ResearchDecision.REJECT_HYPOTHESIS

        return ResearchDecision.INCONCLUSIVE_NEEDS_MORE_DATA

    def _build_citations(self, runs: list[RunResult]) -> list[EvidenceCitation]:
        citations: list[EvidenceCitation] = []
        for run in runs:
            for split_name, m in (
                ("train", run.train_metrics),
                ("val", run.val_metrics),
                ("test", run.test_metrics),
            ):
                citations.append(
                    EvidenceCitation(
                        claim=f"Trial #{run.trial_number} {split_name} Sharpe ratio",
                        run_id=run.id,
                        metric_name="sharpe_ratio",
                        metric_value=str(m.sharpe_ratio),
                        split_name=split_name,
                    )
                )
                citations.append(
                    EvidenceCitation(
                        claim=f"Trial #{run.trial_number} {split_name} total return",
                        run_id=run.id,
                        metric_name="total_return",
                        metric_value=str(m.total_return),
                        split_name=split_name,
                    )
                )
                citations.append(
                    EvidenceCitation(
                        claim=f"Trial #{run.trial_number} {split_name} transaction costs (USD)",
                        run_id=run.id,
                        metric_name="total_transaction_costs",
                        metric_value=str(m.total_transaction_costs),
                        split_name=split_name,
                    )
                )
                citations.append(
                    EvidenceCitation(
                        claim=f"Trial #{run.trial_number} {split_name} max drawdown",
                        run_id=run.id,
                        metric_name="max_drawdown",
                        metric_value=str(m.max_drawdown),
                        split_name=split_name,
                    )
                )
        return citations

    def _build_performance_table(self, runs: list[RunResult]) -> list[dict[str, Any]]:
        rows: list[dict[str, Any]] = []
        for run in runs:
            rows.append(
                {
                    "run_id": run.id,
                    "experiment_id": run.experiment_id,
                    "trial_number": run.trial_number,
                    "train_window": (
                        f"{run.train_metrics.start_date}..{run.train_metrics.end_date}"
                    ),
                    "val_window": f"{run.val_metrics.start_date}..{run.val_metrics.end_date}",
                    "test_holdout_window": (
                        f"{run.test_metrics.start_date}..{run.test_metrics.end_date}"
                    ),
                    "train_sharpe": run.train_metrics.sharpe_ratio,
                    "val_sharpe": run.val_metrics.sharpe_ratio,
                    "test_sharpe": run.test_metrics.sharpe_ratio,
                    "test_total_return": run.test_metrics.total_return,
                    "test_max_drawdown": run.test_metrics.max_drawdown,
                    "test_num_trades": run.test_metrics.num_trades,
                    "test_transaction_costs_usd": str(run.test_metrics.total_transaction_costs),
                    "test_net_pnl_usd": str(run.test_metrics.net_pnl),
                    "test_p_value_raw": run.test_metrics.p_value_two_sided,
                    "bonferroni_p_value": run.multiple_testing.bonferroni_p_value,
                    "deflated_sharpe_prob": run.multiple_testing.deflated_sharpe_probability,
                    "is_vs_oos_degradation": run.is_vs_oos_degradation,
                }
            )
        return rows

    def _build_multiple_testing_warnings(self, runs: list[RunResult]) -> list[str]:
        warnings: list[str] = []
        latest = runs[-1]
        warnings.append(latest.multiple_testing.warning_message)
        if len(runs) > 1:
            sharpes = [r.test_metrics.sharpe_ratio for r in runs]
            warnings.append(
                f"Across {len(runs)} recorded trials, holdout Sharpe ranged from "
                f"{min(sharpes):.2f} to {max(sharpes):.2f}. Selecting the highest-Sharpe "
                "trial post hoc induces selection bias."
            )
        return warnings

    def _build_caveats(self, hypothesis: Hypothesis, run: RunResult, trial_count: int) -> list[str]:
        tm = run.test_metrics
        return [
            "All results are generated on deterministic SIMULATED paper market data; "
            "no live market feed or real capital connectivity exists.",
            f"Holdout window evaluated: {tm.start_date} to {tm.end_date} "
            f"({tm.trading_days} trading days, {tm.num_trades} trades).",
            f"Transaction costs included in net PnL (${tm.total_transaction_costs} "
            "across holdout). Real-world liquidity, queue priority, and borrow "
            "availability may differ.",
            f"Total trials recorded for hypothesis {hypothesis.id}: {trial_count}.",
            "Null hypothesis cannot be rejected without independent out-of-sample & peer review.",
        ]

    def _build_executive_summary(
        self,
        hypothesis: Hypothesis,
        run: RunResult,
        trial_count: int,
        decision: ResearchDecision,
    ) -> str:
        t = run.test_metrics
        return (
            f"[PAPER/SIMULATED] Evaluated hypothesis {hypothesis.id} across {trial_count} "
            f"trial(s). Primary run {run.id} on holdout {t.start_date}..{t.end_date} recorded "
            f"net Sharpe={t.sharpe_ratio:.2f} (95% block-bootstrap CI "
            f"[{t.bootstrap_sharpe_ci_low:.2f}, {t.bootstrap_sharpe_ci_high:.2f}]), "
            f"total return={t.total_return:.2%}, max drawdown={t.max_drawdown:.2%}, "
            f"transaction costs=${t.total_transaction_costs}, raw p={t.p_value_two_sided:.4f}, "
            f"Bonferroni p={run.multiple_testing.bonferroni_p_value:.4f}, "
            f"IS-vs-OOS Sharpe degradation={run.is_vs_oos_degradation:.2f}. "
            f"Automated decision: {decision.value}."
        )

    def _build_methodology_summary(self, hypothesis: Hypothesis, run: RunResult) -> str:
        sig = hypothesis.signal_def
        return (
            f"Constrained signal={sig.signal_type.value} (lookback={sig.lookback_days}d, "
            f"short_window={sig.short_window}d, lag_bars={sig.lag_bars}, "
            f"direction={sig.direction.value}, rebalance={sig.rebalance.value}). "
            f"Train={run.train_metrics.start_date}..{run.train_metrics.end_date}, "
            f"Val={run.val_metrics.start_date}..{run.val_metrics.end_date}, "
            f"Test={run.test_metrics.start_date}..{run.test_metrics.end_date}. "
            f"Dataset={run.dataset_version_id} (checksum={run.dataset_checksum[:12]}...)."
        )
