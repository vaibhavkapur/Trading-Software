"""Unit tests for ExperimentValidator leakage, entitlement, and integrity checks."""

from __future__ import annotations

from datetime import UTC, date, datetime

from quant_agent.data.catalog import DatasetCatalog
from quant_agent.models.domain import DateRange, ValidationIssueSeverity
from quant_agent.models.signals import SignalDefinition, SignalType, TransactionCostAssumptions
from quant_agent.validator.validator import ExperimentValidator
from tests.conftest import make_valid_experiment, make_valid_hypothesis


def _error_codes(result) -> set[str]:
    return {i.code for i in result.issues if i.severity == ValidationIssueSeverity.ERROR}


def test_valid_experiment_passes_all_checks(catalog: DatasetCatalog):
    validator = ExperimentValidator(catalog)
    hyp = make_valid_hypothesis()
    exp = make_valid_experiment(hyp)

    res = validator.validate(exp)
    assert res.passed is True
    assert res.dataset_checksum_verified is True
    assert _error_codes(res) == set()


def test_overlapping_train_val_splits_rejected(catalog: DatasetCatalog):
    validator = ExperimentValidator(catalog)
    hyp = make_valid_hypothesis()
    exp = make_valid_experiment(hyp)
    exp.val_range = DateRange(start=date(2022, 12, 15), end=date(2023, 12, 29))

    res = validator.validate(exp)
    assert res.passed is False
    assert "SPLIT_OVERLAP_LEAKAGE" in _error_codes(res)


def test_insufficient_purge_gap_rejected(catalog: DatasetCatalog):
    validator = ExperimentValidator(catalog)
    hyp = make_valid_hypothesis(horizon_days=10)
    exp = make_valid_experiment(hyp, purge_days=5)
    # Only 3 calendar days between train.end and val.start (< horizon_days=10)
    exp.train_range = DateRange(start=date(2021, 1, 4), end=date(2022, 12, 30))
    exp.val_range = DateRange(start=date(2023, 1, 2), end=date(2023, 12, 29))

    res = validator.validate(exp)
    assert res.passed is False
    assert "INSUFFICIENT_PURGE_GAP" in _error_codes(res)


def test_negative_feature_shift_rejected(catalog: DatasetCatalog):
    validator = ExperimentValidator(catalog)
    hyp = make_valid_hypothesis()
    exp = make_valid_experiment(hyp)
    exp.signal_def = SignalDefinition(
        signal_type=SignalType.ABNORMAL_VOLUME_REVERSAL,
        lookback_days=20,
        short_window=5,
        feature_shift=-1,  # Lookahead!
    )

    res = validator.validate(exp)
    assert res.passed is False
    assert "NEGATIVE_FEATURE_SHIFT" in _error_codes(res)


def test_arbitrary_code_custom_expression_rejected(catalog: DatasetCatalog):
    validator = ExperimentValidator(catalog)
    hyp = make_valid_hypothesis()
    exp = make_valid_experiment(hyp)
    exp.signal_def = SignalDefinition(
        signal_type=SignalType.MEAN_REVERSION_ZSCORE,
        custom_expression="eval('close.shift(-1)')",
    )

    res = validator.validate(exp)
    assert res.passed is False
    codes = _error_codes(res)
    assert "ARBITRARY_CODE_FORBIDDEN" in codes
    assert "FORBIDDEN_CODE_TOKEN" in codes


def test_point_in_time_available_asof_violation_rejected(catalog: DatasetCatalog):
    validator = ExperimentValidator(catalog)
    hyp = make_valid_hypothesis()
    exp = make_valid_experiment(hyp)
    # Dataset available_asof is 2025-01-02; pretend experiment ran in 2023
    exp.asof_timestamp_utc = datetime(2023, 6, 1, 0, 0, 0, tzinfo=UTC)

    res = validator.validate(exp)
    assert res.passed is False
    assert "POINT_IN_TIME_VIOLATION" in _error_codes(res)


def test_entitlement_mismatch_and_unknown_dataset_rejected(catalog: DatasetCatalog):
    validator = ExperimentValidator(catalog)
    hyp = make_valid_hypothesis(dataset_id="sim_institutional_alt_v1")
    exp = make_valid_experiment(hyp, dataset_version_id="sim_institutional_alt_v1")
    exp.universe = ["AAPL_SIM", "MSFT_SIM"]
    exp.entitlement_tier = "simulated_internal"  # dataset requires restricted_licensed

    res = validator.validate(exp)
    assert res.passed is False
    assert "ENTITLEMENT_DENIED" in _error_codes(res)

    exp.dataset_version_id = "unlisted_external_feed"
    res2 = validator.validate(exp)
    assert res2.passed is False
    assert "UNKNOWN_DATASET" in _error_codes(res2)


def test_zero_costs_emits_warning_and_trial_budget_enforced(catalog: DatasetCatalog):
    validator = ExperimentValidator(catalog)
    hyp = make_valid_hypothesis()
    exp = make_valid_experiment(hyp)
    exp.cost_assumptions = TransactionCostAssumptions(
        commission_bps=0.0, half_spread_bps=0.0, impact_bps_per_pct_adv=0.0
    )
    exp.max_trials_budget = 3

    res_ok = validator.validate(exp, existing_trial_count=1)
    assert res_ok.passed is True
    warn_codes = {i.code for i in res_ok.issues if i.severity == ValidationIssueSeverity.WARNING}
    assert "ZERO_TRANSACTION_COSTS_WARNING" in warn_codes
    assert "MULTIPLE_TRIALS_WARNING" in warn_codes

    res_exceeded = validator.validate(exp, existing_trial_count=3)
    assert res_exceeded.passed is False
    assert "MULTIPLE_TESTING_BUDGET_EXCEEDED" in _error_codes(res_exceeded)
