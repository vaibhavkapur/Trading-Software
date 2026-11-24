"""Unit tests for statistical metrics, block bootstrap, and multiple-testing corrections."""

from __future__ import annotations

import random

from quant_agent.backtest.stats import (
    compute_deflated_sharpe_probability,
    compute_is_vs_oos_degradation,
    compute_max_drawdown,
    compute_sharpe,
    compute_t_stat_and_pvalue,
    evaluate_multiple_testing,
    stationary_block_bootstrap,
)


def test_sharpe_drawdown_and_tstat_on_positive_drift_series():
    rng = random.Random(123)
    returns = [0.002 + rng.gauss(0.0, 0.008) for _ in range(252)]
    sharpe = compute_sharpe(returns)
    t_stat, p_val = compute_t_stat_and_pvalue(returns)
    assert sharpe > 1.0
    assert t_stat > 2.0
    assert p_val < 0.05

    equity = [100.0, 120.0, 90.0, 110.0]
    assert abs(compute_max_drawdown(equity) - 0.25) < 1e-9


def test_stationary_block_bootstrap_is_deterministic_and_ordered():
    rng = random.Random(99)
    returns = [0.001 + rng.gauss(0.0, 0.01) for _ in range(200)]
    b1 = stationary_block_bootstrap(returns, num_resamples=200, avg_block_len=5, seed=7)
    b2 = stationary_block_bootstrap(returns, num_resamples=200, avg_block_len=5, seed=7)
    assert b1 == b2
    assert b1.sharpe_low <= b1.sharpe_high
    assert b1.mean_return_low <= b1.mean_return_high


def test_multiple_testing_penalizes_more_trials():
    rng = random.Random(55)
    returns = [0.0007 + rng.gauss(0.0, 0.01) for _ in range(252)]
    _, raw_p = compute_t_stat_and_pvalue(returns)

    dsr_1 = compute_deflated_sharpe_probability(returns, num_trials=1)
    dsr_25 = compute_deflated_sharpe_probability(returns, num_trials=25)
    assert dsr_25 < dsr_1

    mt_multi = evaluate_multiple_testing(
        daily_returns=returns,
        raw_p_value=raw_p,
        all_trial_p_values=[0.4, 0.3, 0.2, raw_p],
        trial_sharpes=[0.1, 0.4, 0.6, compute_sharpe(returns)],
    )
    assert mt_multi.trial_count == 4
    assert mt_multi.bonferroni_p_value >= raw_p
    assert mt_multi.warning_triggered is True
    assert "Multiple-testing caution" in mt_multi.warning_message


def test_is_vs_oos_degradation():
    assert compute_is_vs_oos_degradation(1.8, 0.4) == 1.4
    assert compute_is_vs_oos_degradation(0.5, 0.9) == -0.4
