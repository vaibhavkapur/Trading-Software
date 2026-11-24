"""Experiment Validator — strict leakage, entitlement, integrity, and data-hygiene gate.

Every experiment MUST pass validation before it can be queued or executed.
Checks are versioned (`validator_version`) and return structured issues so both
automated workflows and human researchers can diagnose rejections deterministically.
"""

from __future__ import annotations

from datetime import timedelta

from quant_agent.data.catalog import DatasetCatalog
from quant_agent.models.domain import (
    Experiment,
    ValidationIssue,
    ValidationIssueSeverity,
    ValidationResult,
)
from quant_agent.models.signals import FORBIDDEN_CODE_TOKENS


class ExperimentValidator:
    """Deterministic pre-execution validator preventing lookahead and data leakage."""

    def __init__(
        self,
        catalog: DatasetCatalog,
        *,
        validator_version: str = "1.0.0",
        max_clock_skew_seconds: int = 300,
        max_stale_bar_ratio: float = 0.05,
        max_missing_bar_ratio: float = 0.15,
    ) -> None:
        self.catalog = catalog
        self.validator_version = validator_version
        self.max_clock_skew_seconds = max_clock_skew_seconds
        self.max_stale_bar_ratio = max_stale_bar_ratio
        self.max_missing_bar_ratio = max_missing_bar_ratio

    def validate(
        self,
        experiment: Experiment,
        *,
        existing_trial_count: int = 0,
    ) -> ValidationResult:
        issues: list[ValidationIssue] = []
        checks_run: list[str] = []
        checksum_ok = False

        # 1. No arbitrary code / DSL safety check
        checks_run.append("dsl_code_safety")
        sig = experiment.signal_def
        if sig.custom_expression is not None and sig.custom_expression.strip():
            issues.append(
                ValidationIssue(
                    code="ARBITRARY_CODE_FORBIDDEN",
                    severity=ValidationIssueSeverity.ERROR,
                    field="signal_def.custom_expression",
                    message=(
                        "Arbitrary code or custom expressions are forbidden in MVP; "
                        "use allowlisted SignalType definitions only."
                    ),
                )
            )
            lowered = sig.custom_expression.lower()
            for token in FORBIDDEN_CODE_TOKENS:
                if token in lowered:
                    issues.append(
                        ValidationIssue(
                            code="FORBIDDEN_CODE_TOKEN",
                            severity=ValidationIssueSeverity.ERROR,
                            field="signal_def.custom_expression",
                            message=f"Detected forbidden token {token!r} in signal specification.",
                        )
                    )
                    break

        # 2. Signal lag & feature shift lookahead barrier
        checks_run.append("signal_causality")
        if sig.feature_shift < 0:
            issues.append(
                ValidationIssue(
                    code="NEGATIVE_FEATURE_SHIFT",
                    severity=ValidationIssueSeverity.ERROR,
                    field="signal_def.feature_shift",
                    message=(
                        f"feature_shift={sig.feature_shift} uses future observations; must be >= 0."
                    ),
                )
            )
        if sig.lag_bars < 1:
            issues.append(
                ValidationIssue(
                    code="INVALID_EXECUTION_LAG",
                    severity=ValidationIssueSeverity.ERROR,
                    field="signal_def.lag_bars",
                    message="lag_bars must be >= 1 to prevent same-bar close lookahead.",
                )
            )
        if sig.short_window >= sig.lookback_days:
            issues.append(
                ValidationIssue(
                    code="SHORT_WINDOW_EXCEEDS_LOOKBACK",
                    severity=ValidationIssueSeverity.ERROR,
                    field="signal_def.short_window",
                    message=(
                        f"short_window ({sig.short_window}) must be strictly less than "
                        f"lookback_days ({sig.lookback_days})."
                    ),
                )
            )

        # 3. Date ranges & chronological leakage checks (train < val < test with purge/horizon gap)
        checks_run.append("chronological_split_isolation")
        for name, dr in (
            ("train_range", experiment.train_range),
            ("val_range", experiment.val_range),
            ("test_range", experiment.test_range),
        ):
            if dr.start >= dr.end:
                issues.append(
                    ValidationIssue(
                        code="INVALID_DATE_RANGE",
                        severity=ValidationIssueSeverity.ERROR,
                        field=name,
                        message=f"{name} start ({dr.start}) must be before end ({dr.end}).",
                    )
                )

        required_gap_days = max(experiment.purge_days, experiment.horizon_days)

        train_val_gap = (experiment.val_range.start - experiment.train_range.end).days
        if train_val_gap <= 0:
            issues.append(
                ValidationIssue(
                    code="SPLIT_OVERLAP_LEAKAGE",
                    severity=ValidationIssueSeverity.ERROR,
                    field="val_range.start",
                    message=(
                        f"Validation split ({experiment.val_range.start}) overlaps or precedes "
                        f"train end ({experiment.train_range.end})."
                    ),
                )
            )
        elif train_val_gap < required_gap_days:
            issues.append(
                ValidationIssue(
                    code="INSUFFICIENT_PURGE_GAP",
                    severity=ValidationIssueSeverity.ERROR,
                    field="purge_days",
                    message=(
                        f"Gap between train_range.end and val_range.start is {train_val_gap}d, "
                        f"which is smaller than required purge/horizon gap ({required_gap_days}d)."
                    ),
                )
            )

        val_test_gap = (experiment.test_range.start - experiment.val_range.end).days
        if val_test_gap <= 0:
            issues.append(
                ValidationIssue(
                    code="SPLIT_OVERLAP_LEAKAGE",
                    severity=ValidationIssueSeverity.ERROR,
                    field="test_range.start",
                    message=(
                        f"Test holdout ({experiment.test_range.start}) overlaps or precedes "
                        f"validation end ({experiment.val_range.end})."
                    ),
                )
            )
        elif val_test_gap < required_gap_days:
            issues.append(
                ValidationIssue(
                    code="INSUFFICIENT_PURGE_GAP",
                    severity=ValidationIssueSeverity.ERROR,
                    field="purge_days",
                    message=(
                        f"Gap between val_range.end and test_range.start is {val_test_gap}d, "
                        f"which is smaller than required purge/horizon gap ({required_gap_days}d)."
                    ),
                )
            )

        # 4. Dataset allowlist, entitlement, point-in-time availability, and checksum
        checks_run.append("dataset_allowlist_and_entitlement")
        ds = self.catalog.get_version(experiment.dataset_version_id)
        if ds is None:
            issues.append(
                ValidationIssue(
                    code="UNKNOWN_DATASET",
                    severity=ValidationIssueSeverity.ERROR,
                    field="dataset_version_id",
                    message=f"Dataset {experiment.dataset_version_id!r} is not registered.",
                )
            )
        else:
            if not ds.is_allowlisted:
                issues.append(
                    ValidationIssue(
                        code="NON_ALLOWLISTED_DATASET",
                        severity=ValidationIssueSeverity.ERROR,
                        field="dataset_version_id",
                        message=f"Dataset {ds.id!r} is not allowlisted for research execution.",
                    )
                )
            if ds.entitlement_tier != experiment.entitlement_tier:
                issues.append(
                    ValidationIssue(
                        code="ENTITLEMENT_DENIED",
                        severity=ValidationIssueSeverity.ERROR,
                        field="entitlement_tier",
                        message=(
                            f"Experiment entitlement tier {experiment.entitlement_tier!r} does "
                            f"not match dataset tier {ds.entitlement_tier!r}."
                        ),
                    )
                )

            checks_run.append("dataset_checksum")
            checksum_ok = self.catalog.verify_checksum(ds.id)
            if not checksum_ok:
                issues.append(
                    ValidationIssue(
                        code="CHECKSUM_MISMATCH",
                        severity=ValidationIssueSeverity.ERROR,
                        field="dataset_version_id",
                        message=(
                            f"Dataset {ds.id!r} failed SHA-256 checksum integrity verification."
                        ),
                    )
                )

            checks_run.append("point_in_time_availability")
            if ds.available_asof > experiment.asof_timestamp_utc:
                issues.append(
                    ValidationIssue(
                        code="POINT_IN_TIME_VIOLATION",
                        severity=ValidationIssueSeverity.ERROR,
                        field="asof_timestamp_utc",
                        message=(
                            f"Dataset {ds.id!r} became available at "
                            f"{ds.available_asof.isoformat()}, after experiment as-of "
                            f"{experiment.asof_timestamp_utc.isoformat()}."
                        ),
                    )
                )

            if experiment.train_range.start < ds.coverage_start:
                issues.append(
                    ValidationIssue(
                        code="COVERAGE_UNDERFLOW",
                        severity=ValidationIssueSeverity.ERROR,
                        field="train_range.start",
                        message=(
                            f"train_range.start ({experiment.train_range.start}) precedes "
                            f"dataset coverage_start ({ds.coverage_start})."
                        ),
                    )
                )
            if experiment.test_range.end > ds.coverage_end:
                issues.append(
                    ValidationIssue(
                        code="COVERAGE_OVERFLOW",
                        severity=ValidationIssueSeverity.ERROR,
                        field="test_range.end",
                        message=(
                            f"test_range.end ({experiment.test_range.end}) exceeds "
                            f"dataset coverage_end ({ds.coverage_end})."
                        ),
                    )
                )

            # 5. Universe membership
            checks_run.append("universe_membership")
            available_symbols = set(ds.symbols)
            missing_syms = sorted(set(experiment.universe) - available_symbols)
            if missing_syms:
                issues.append(
                    ValidationIssue(
                        code="UNIVERSE_SYMBOL_MISSING",
                        severity=ValidationIssueSeverity.ERROR,
                        field="universe",
                        message=f"Symbols not in dataset {ds.id!r}: {missing_syms}",
                    )
                )

            # 6. Data hygiene: warmup length, staleness, clock skew, duplicates, gaps
            if not missing_syms and ds.is_allowlisted:
                checks_run.append("data_quality_and_clock_hygiene")
                self._check_data_quality(experiment, ds.id, issues)

        # 7. Transaction cost sanity check
        checks_run.append("transaction_cost_sanity")
        costs = experiment.cost_assumptions
        if (
            costs.commission_bps == 0.0
            and costs.half_spread_bps == 0.0
            and costs.impact_bps_per_pct_adv == 0.0
        ):
            issues.append(
                ValidationIssue(
                    code="ZERO_TRANSACTION_COSTS_WARNING",
                    severity=ValidationIssueSeverity.WARNING,
                    field="cost_assumptions",
                    message=(
                        "All transaction cost assumptions are 0 bps; backtest returns will "
                        "overstate net realizable performance."
                    ),
                )
            )

        # 8. Trial budget / multiple-testing check
        checks_run.append("multiple_testing_budget")
        if existing_trial_count >= experiment.max_trials_budget:
            issues.append(
                ValidationIssue(
                    code="MULTIPLE_TESTING_BUDGET_EXCEEDED",
                    severity=ValidationIssueSeverity.ERROR,
                    field="max_trials_budget",
                    message=(
                        f"Hypothesis already has {existing_trial_count} completed trials, "
                        f"reaching max_trials_budget={experiment.max_trials_budget}. "
                        "Raise budget explicitly or formulate a new hypothesis to avoid p-hacking."
                    ),
                )
            )
        elif existing_trial_count >= 1:
            issues.append(
                ValidationIssue(
                    code="MULTIPLE_TRIALS_WARNING",
                    severity=ValidationIssueSeverity.WARNING,
                    field="hypothesis_id",
                    message=(
                        f"Trial #{existing_trial_count + 1} on this hypothesis; "
                        "multiple-testing corrections will be applied to reported metrics."
                    ),
                )
            )

        has_errors = any(i.severity == ValidationIssueSeverity.ERROR for i in issues)
        return ValidationResult(
            experiment_id=experiment.id,
            passed=not has_errors,
            validator_version=self.validator_version,
            dataset_checksum_verified=checksum_ok,
            issues=issues,
            checks_run=checks_run,
        )

    def _check_data_quality(
        self,
        experiment: Experiment,
        dataset_id: str,
        issues: list[ValidationIssue],
    ) -> None:
        bars_by_sym = self.catalog.load_bars(
            dataset_id,
            experiment.universe,
            start=experiment.train_range.start,
            end=experiment.test_range.end,
        )
        min_warmup = experiment.signal_def.lookback_days + experiment.signal_def.lag_bars + 5

        total_bars = 0
        stale_bars = 0
        skew_violations = 0
        duplicate_count = 0

        # Estimate expected weekdays in overall window
        expected_weekdays = 0
        cur = experiment.train_range.start
        while cur <= experiment.test_range.end:
            if cur.weekday() < 5:
                expected_weekdays += 1
            cur += timedelta(days=1)

        for sym, bars in bars_by_sym.items():
            seen_dates = set()
            train_bars = 0
            for bar in bars:
                total_bars += 1
                if bar.trading_date in seen_dates:
                    duplicate_count += 1
                else:
                    seen_dates.add(bar.trading_date)
                if bar.is_stale or bar.volume == 0:
                    stale_bars += 1
                delta_sec = (bar.receive_time_utc - bar.event_time_utc).total_seconds()
                if delta_sec < 0 or delta_sec > self.max_clock_skew_seconds:
                    skew_violations += 1
                if experiment.train_range.start <= bar.trading_date <= experiment.train_range.end:
                    train_bars += 1

            if train_bars < min_warmup:
                issues.append(
                    ValidationIssue(
                        code="INSUFFICIENT_WARMUP_DATA",
                        severity=ValidationIssueSeverity.ERROR,
                        field="train_range",
                        message=(
                            f"Symbol {sym} has only {train_bars} bars in train_range; "
                            f"need at least {min_warmup} bars for lookback+lag warmup."
                        ),
                    )
                )
                break

            if expected_weekdays > 0:
                missing_ratio = max(0.0, 1.0 - (len(seen_dates) / expected_weekdays))
                if missing_ratio > self.max_missing_bar_ratio:
                    issues.append(
                        ValidationIssue(
                            code="MISSING_DATA_GAPS",
                            severity=ValidationIssueSeverity.ERROR,
                            field="dataset_version_id",
                            message=(
                                f"Symbol {sym} is missing {missing_ratio:.1%} of expected "
                                f"trading bars (limit {self.max_missing_bar_ratio:.1%})."
                            ),
                        )
                    )
                    break

        if duplicate_count > 0:
            issues.append(
                ValidationIssue(
                    code="DUPLICATE_BARS_DETECTED",
                    severity=ValidationIssueSeverity.ERROR,
                    field="dataset_version_id",
                    message=(
                        f"Detected {duplicate_count} duplicate trading-date bars in dataset "
                        f"{dataset_id!r}."
                    ),
                )
            )

        if skew_violations > 0:
            issues.append(
                ValidationIssue(
                    code="CLOCK_SKEW_VIOLATION",
                    severity=ValidationIssueSeverity.ERROR,
                    field="dataset_version_id",
                    message=(
                        f"Detected {skew_violations} bars with invalid event_time vs "
                        f"receive_time skew (allowed 0..{self.max_clock_skew_seconds}s)."
                    ),
                )
            )

        if total_bars > 0:
            stale_ratio = stale_bars / total_bars
            if stale_ratio > self.max_stale_bar_ratio:
                issues.append(
                    ValidationIssue(
                        code="DATA_STALE_BARS",
                        severity=ValidationIssueSeverity.ERROR,
                        field="dataset_version_id",
                        message=(
                            f"Dataset contains {stale_ratio:.1%} stale/zero-volume bars "
                            f"(limit {self.max_stale_bar_ratio:.1%})."
                        ),
                    )
                )
