"""Statistical performance metrics, dependence-aware bootstrap, and multiple-testing corrections.

Provides:
- Annualized return, volatility, Sharpe, Sortino, Max Drawdown, Calmar
- Two-sided t-statistic and normal/t-approximation p-value
- Stationary/circular block bootstrap confidence intervals (dependence-aware for return series)
- Bonferroni, Benjamini-Hochberg FDR, and Deflated Sharpe Ratio (Bailey & Lopez de Prado)
- In-sample vs out-of-sample degradation metric
"""

from __future__ import annotations

import math
import random
from dataclasses import dataclass

from quant_agent.models.domain import MultipleTestingSummary

TRADING_DAYS_PER_YEAR = 252.0
EULER_MASCHERONI = 0.5772156649015329


def _norm_cdf(x: float) -> float:
    return 0.5 * (1.0 + math.erf(x / math.sqrt(2.0)))


def _norm_ppf(p: float) -> float:
    """Acklam's rational approximation for standard normal inverse CDF."""
    p = min(max(p, 1e-12), 1.0 - 1e-12)
    a = (
        -3.969683028665376e01,
        2.209460984245205e02,
        -2.759285104469687e02,
        1.383577518672690e02,
        -3.066479806614716e01,
        2.506628277459239e00,
    )
    b = (
        -5.447609879822406e01,
        1.615858368580409e02,
        -1.556989798598866e02,
        6.680131188771972e01,
        -1.328068155288572e01,
    )
    c = (
        -7.784894002430293e-03,
        -3.223964580411365e-01,
        -2.400758277161838e00,
        -2.549732539343734e00,
        4.374664141464968e00,
        2.938163982698783e00,
    )
    d = (
        7.784695709041462e-03,
        3.224671290700398e-01,
        2.445134137142996e00,
        3.754408661907416e00,
    )
    plow = 0.02425
    phigh = 1.0 - plow
    if p < plow:
        q = math.sqrt(-2.0 * math.log(p))
        return (((((c[0] * q + c[1]) * q + c[2]) * q + c[3]) * q + c[4]) * q + c[5]) / (
            (((d[0] * q + d[1]) * q + d[2]) * q + d[3]) * q + 1.0
        )
    if p <= phigh:
        q = p - 0.5
        r = q * q
        return (
            (((((a[0] * r + a[1]) * r + a[2]) * r + a[3]) * r + a[4]) * r + a[5])
            * q
            / (((((b[0] * r + b[1]) * r + b[2]) * r + b[3]) * r + b[4]) * r + 1.0)
        )
    q = math.sqrt(-2.0 * math.log(1.0 - p))
    return -(((((c[0] * q + c[1]) * q + c[2]) * q + c[3]) * q + c[4]) * q + c[5]) / (
        (((d[0] * q + d[1]) * q + d[2]) * q + d[3]) * q + 1.0
    )


def sample_mean(xs: list[float]) -> float:
    return sum(xs) / len(xs) if xs else 0.0


def sample_std(xs: list[float]) -> float:
    n = len(xs)
    if n < 2:
        return 0.0
    mu = sum(xs) / n
    var = sum((x - mu) ** 2 for x in xs) / (n - 1)
    return math.sqrt(max(0.0, var))


def compute_sharpe(daily_returns: list[float]) -> float:
    if len(daily_returns) < 2:
        return 0.0
    sd = sample_std(daily_returns)
    if sd <= 1e-12:
        return 0.0
    return (sample_mean(daily_returns) / sd) * math.sqrt(TRADING_DAYS_PER_YEAR)


def compute_sortino(daily_returns: list[float]) -> float:
    if len(daily_returns) < 2:
        return 0.0
    downside = [min(0.0, r) ** 2 for r in daily_returns]
    downside_dev = math.sqrt(sum(downside) / len(daily_returns))
    if downside_dev <= 1e-12:
        return 0.0
    return (sample_mean(daily_returns) / downside_dev) * math.sqrt(TRADING_DAYS_PER_YEAR)


def compute_max_drawdown(equity_curve: list[float]) -> float:
    """Return max peak-to-trough drawdown as a positive fraction in [0, 1]."""
    if not equity_curve:
        return 0.0
    peak = equity_curve[0]
    max_dd = 0.0
    for val in equity_curve:
        if val > peak:
            peak = val
        if peak > 0:
            dd = (peak - val) / peak
            if dd > max_dd:
                max_dd = dd
    return max_dd


def compute_t_stat_and_pvalue(daily_returns: list[float]) -> tuple[float, float]:
    """Two-sided t-test of H0: mean daily return == 0."""
    n = len(daily_returns)
    if n < 3:
        return 0.0, 1.0
    sd = sample_std(daily_returns)
    if sd <= 1e-12:
        return 0.0, 1.0
    mu = sample_mean(daily_returns)
    t_stat = mu / (sd / math.sqrt(n))
    # For n >= 30 normal approximation is tight; apply mild finite-sample correction for smaller n
    df = max(1, n - 1)
    z_equiv = t_stat * (1.0 - 1.0 / (4.0 * df))
    p_val = max(0.0, min(1.0, 2.0 * (1.0 - _norm_cdf(abs(z_equiv)))))
    return t_stat, p_val


@dataclass(frozen=True)
class BootstrapCI:
    sharpe_low: float
    sharpe_high: float
    mean_return_low: float
    mean_return_high: float


def stationary_block_bootstrap(
    daily_returns: list[float],
    *,
    num_resamples: int = 300,
    avg_block_len: int = 5,
    seed: int = 42,
) -> BootstrapCI:
    """Politis-Romano stationary block bootstrap preserving serial dependence."""
    n = len(daily_returns)
    if n < 5:
        return BootstrapCI(0.0, 0.0, 0.0, 0.0)

    rng = random.Random(seed)
    p_new_block = 1.0 / max(1, avg_block_len)
    sharpes: list[float] = []
    means: list[float] = []

    for _ in range(num_resamples):
        sample: list[float] = []
        idx = rng.randrange(n)
        for _ in range(n):
            sample.append(daily_returns[idx])
            if rng.random() < p_new_block:
                idx = rng.randrange(n)
            else:
                idx = (idx + 1) % n
        sharpes.append(compute_sharpe(sample))
        means.append(sample_mean(sample) * TRADING_DAYS_PER_YEAR)

    sharpes.sort()
    means.sort()
    lo_idx = max(0, int(0.025 * len(sharpes)))
    hi_idx = min(len(sharpes) - 1, int(0.975 * len(sharpes)))
    return BootstrapCI(
        sharpe_low=round(sharpes[lo_idx], 4),
        sharpe_high=round(sharpes[hi_idx], 4),
        mean_return_low=round(means[lo_idx], 4),
        mean_return_high=round(means[hi_idx], 4),
    )


def compute_skew_kurtosis(xs: list[float]) -> tuple[float, float]:
    n = len(xs)
    if n < 4:
        return 0.0, 3.0
    mu = sample_mean(xs)
    sd = sample_std(xs)
    if sd <= 1e-12:
        return 0.0, 3.0
    m3 = sum(((x - mu) / sd) ** 3 for x in xs) / n
    m4 = sum(((x - mu) / sd) ** 4 for x in xs) / n
    return m3, max(1.0, m4)


def compute_deflated_sharpe_probability(
    daily_returns: list[float],
    *,
    num_trials: int,
    sharpe_variance_across_trials: float = 0.5,
) -> float:
    """Deflated Sharpe Ratio probability PSR(SR*) accounting for trial count & non-normality."""
    n = len(daily_returns)
    if n < 5:
        return 0.5
    sr_daily = sample_mean(daily_returns) / max(1e-12, sample_std(daily_returns))
    skew, kurt = compute_skew_kurtosis(daily_returns)

    if num_trials <= 1:
        sr_star_daily = 0.0
    else:
        # Expected maximum Sharpe under null across N trials (in annualized units, then daily)
        z1 = _norm_ppf(1.0 - 1.0 / num_trials)
        z2 = _norm_ppf(1.0 - 1.0 / (num_trials * math.e))
        expected_max_ann = math.sqrt(max(1e-6, sharpe_variance_across_trials)) * (
            (1.0 - EULER_MASCHERONI) * z1 + EULER_MASCHERONI * z2
        )
        sr_star_daily = expected_max_ann / math.sqrt(TRADING_DAYS_PER_YEAR)

    denom_sq = max(
        1e-9,
        1.0 - skew * sr_daily + ((kurt - 1.0) / 4.0) * (sr_daily**2),
    )
    z_score = ((sr_daily - sr_star_daily) * math.sqrt(max(1, n - 1))) / math.sqrt(denom_sq)
    return round(_norm_cdf(z_score), 4)


def evaluate_multiple_testing(
    *,
    daily_returns: list[float],
    raw_p_value: float,
    all_trial_p_values: list[float],
    trial_sharpes: list[float],
) -> MultipleTestingSummary:
    """Compute Bonferroni, Benjamini-Hochberg FDR, and Deflated Sharpe across trials."""
    p_values = list(all_trial_p_values)
    if not p_values:
        p_values = [raw_p_value]
    trial_count = len(p_values)

    bonferroni = min(1.0, raw_p_value * trial_count)

    # Benjamini-Hochberg FDR adjusted p-value for the current trial's raw_p_value
    indexed = sorted(enumerate(p_values), key=lambda pair: pair[1])
    bh_adjusted = [1.0] * trial_count
    running_min = 1.0
    for rank_rev in range(trial_count, 0, -1):
        orig_idx, pval = indexed[rank_rev - 1]
        adj = min(running_min, (pval * trial_count) / rank_rev)
        running_min = min(1.0, adj)
        bh_adjusted[orig_idx] = running_min
    bh_current = bh_adjusted[-1]

    sr_var = (
        sample_std(trial_sharpes) ** 2
        if len(trial_sharpes) >= 2 and sample_std(trial_sharpes) > 0.05
        else 0.5
    )
    dsr_prob = compute_deflated_sharpe_probability(
        daily_returns,
        num_trials=trial_count,
        sharpe_variance_across_trials=sr_var,
    )

    warning_triggered = trial_count > 1 or dsr_prob < 0.90 or bonferroni > 0.05
    if trial_count > 1:
        warning_msg = (
            f"Multiple-testing caution: {trial_count} trials evaluated on this hypothesis. "
            f"Raw p={raw_p_value:.4f}, Bonferroni p={bonferroni:.4f}, "
            f"BH-FDR p={bh_current:.4f}, Deflated Sharpe prob={dsr_prob:.2%}. "
            "Do not select top-performing runs post hoc without out-of-sample confirmation."
        )
    else:
        warning_msg = (
            f"Single trial evaluated (n=1). Raw p={raw_p_value:.4f}, "
            f"Deflated Sharpe prob={dsr_prob:.2%}. Confirm stability across regimes before review."
        )

    return MultipleTestingSummary(
        trial_count=trial_count,
        raw_p_value=round(raw_p_value, 6),
        bonferroni_p_value=round(bonferroni, 6),
        bh_fdr_adjusted_p_value=round(bh_current, 6),
        deflated_sharpe_probability=dsr_prob,
        warning_triggered=warning_triggered,
        warning_message=warning_msg,
    )


def compute_is_vs_oos_degradation(train_sharpe: float, test_sharpe: float) -> float:
    """Measure out-of-sample degradation relative to in-sample Sharpe.

    Positive values indicate degradation (OOS worse than IS).
    """
    return round(train_sharpe - test_sharpe, 4)
