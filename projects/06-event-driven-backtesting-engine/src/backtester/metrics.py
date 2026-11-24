"""Performance and execution metrics.

Ratios are analytics, not money, so they are floats; the inputs they are computed from
(equity, fills, fees) are exact integers. Return statistics use one observation per UTC
day (the last equity of each day) and are annualised with ``periods_per_year``. A metric
that is undefined for the sample, such as a Sharpe ratio with fewer than two returns or
zero variance, is reported as None rather than as zero or infinity.

Definitions
  total_return      final equity / starting equity - 1
  sharpe            mean(excess daily return) / stdev(daily return, n-1) * sqrt(periods)
  sortino           mean(excess daily return) / sqrt(mean(min(excess, 0)^2)) * sqrt(periods)
  max_drawdown      largest peak-to-trough fall in equity, over every snapshot
  turnover          total traded notional (buys plus sells) / mean daily equity
  shortfall         sum over fills of side * (fill price - decision price) * quantity,
                    where the decision price is the last price visible to the strategy
                    when it sent the order. Positive is a cost. Fees are reported apart,
                    and the opportunity cost of unfilled quantity is not included.
"""

from __future__ import annotations

import math
from collections.abc import Iterable, Mapping, Sequence
from dataclasses import dataclass

from .orders import SimFill
from .portfolio import PortfolioSnapshot
from .timeutil import day_index

# A per-period deviation below this is rounding noise, not risk. Without the floor, a
# series of identical returns could report an astronomically large ratio.
_NO_VARIATION = 1e-12


@dataclass(frozen=True, slots=True)
class ReturnStats:
    periods: int
    total_return: float
    annualized_return: float | None
    annualized_volatility: float | None
    sharpe: float | None
    sortino: float | None
    max_drawdown: float


@dataclass(frozen=True, slots=True)
class Metrics:
    start_ts: int | None
    end_ts: int | None
    periods: int
    initial_equity: int
    final_equity: int
    pnl: int
    total_return: float
    annualized_return: float | None
    annualized_volatility: float | None
    sharpe: float | None
    sortino: float | None
    max_drawdown: float
    max_drawdown_peak_ts: int | None
    max_drawdown_trough_ts: int | None
    traded_notional: int
    turnover: float | None
    fees: int
    fill_count: int
    shortfall: int
    shortfall_bps: float | None
    min_cash: int


def return_stats(
    returns: Sequence[float], periods_per_year: int, risk_free_rate: float = 0.0
) -> ReturnStats:
    """Statistics of a series of per-period simple returns."""
    n = len(returns)
    growth = 1.0
    peak = 1.0
    max_dd = 0.0
    for r in returns:
        growth *= 1.0 + r
        if growth > peak:
            peak = growth
        elif peak > 0:
            max_dd = max(max_dd, (peak - growth) / peak)
    total = growth - 1.0

    annualized = None
    if n > 0 and growth > 0:
        annualized = growth ** (periods_per_year / n) - 1.0

    volatility = sharpe = sortino = None
    if n >= 2:
        rf = risk_free_rate / periods_per_year
        mean = math.fsum(returns) / n
        variance = math.fsum((r - mean) ** 2 for r in returns) / (n - 1)
        stdev = math.sqrt(variance)
        scale = math.sqrt(periods_per_year)
        volatility = stdev * scale
        if stdev > _NO_VARIATION:
            sharpe = (mean - rf) / stdev * scale
        downside = math.sqrt(math.fsum(min(r - rf, 0.0) ** 2 for r in returns) / n)
        if downside > _NO_VARIATION:
            sortino = (mean - rf) / downside * scale
    return ReturnStats(n, total, annualized, volatility, sharpe, sortino, max_dd)


def daily_closes(
    snapshots: Iterable[PortfolioSnapshot], start: int | None = None, end: int | None = None
) -> list[tuple[int, int]]:
    """(UTC day index, last equity of that day) for snapshots in [start, end)."""
    closes: list[tuple[int, int]] = []
    for snap in snapshots:
        if (start is not None and snap.ts < start) or (end is not None and snap.ts >= end):
            continue
        day = day_index(snap.ts)
        if closes and closes[-1][0] == day:
            closes[-1] = (day, snap.equity)
        else:
            closes.append((day, snap.equity))
    return closes


def daily_returns(
    snapshots: Iterable[PortfolioSnapshot],
    base_equity: int,
    start: int | None = None,
    end: int | None = None,
) -> list[tuple[int, float]]:
    """(UTC day index, simple daily return), the first measured from ``base_equity``."""
    out: list[tuple[int, float]] = []
    previous = base_equity
    for day, equity in daily_closes(snapshots, start, end):
        # A wiped-out account has no meaningful return; report zero rather than divide.
        out.append((day, equity / previous - 1.0 if previous > 0 else 0.0))
        previous = equity
    return out


def compute_metrics(
    snapshots: Sequence[PortfolioSnapshot],
    fills: Iterable[SimFill],
    decision_px: Mapping[str, int | None],
    *,
    base_equity: int,
    periods_per_year: int,
    risk_free_rate: float = 0.0,
    start: int | None = None,
    end: int | None = None,
) -> Metrics:
    """Metrics for the half-open window [start, end), starting from ``base_equity``."""
    window = [
        s for s in snapshots if (start is None or s.ts >= start) and (end is None or s.ts < end)
    ]
    returns = daily_returns(window, base_equity)
    stats = return_stats([r for _, r in returns], periods_per_year, risk_free_rate)

    final_equity = window[-1].equity if window else base_equity
    peak = base_equity
    peak_ts: int | None = None
    max_dd = 0.0
    dd_peak_ts: int | None = None
    dd_trough_ts: int | None = None
    for snap in window:
        if snap.equity > peak:
            peak, peak_ts = snap.equity, snap.ts
        elif peak > 0:
            drawdown = (peak - snap.equity) / peak
            if drawdown > max_dd:
                max_dd, dd_peak_ts, dd_trough_ts = drawdown, peak_ts, snap.ts

    traded = fees = shortfall = decision_notional = count = 0
    for fill in fills:
        if (start is not None and fill.ts < start) or (end is not None and fill.ts >= end):
            continue
        count += 1
        traded += fill.qty * fill.px
        fees += fill.fee
        decided = decision_px.get(fill.order_id)
        if decided:
            shortfall += fill.side.sign * (fill.px - decided) * fill.qty
            decision_notional += decided * fill.qty

    closes = daily_closes(window)
    mean_equity = math.fsum(e for _, e in closes) / len(closes) if closes else base_equity

    return Metrics(
        start_ts=window[0].ts if window else start,
        end_ts=window[-1].ts if window else end,
        periods=stats.periods,
        initial_equity=base_equity,
        final_equity=final_equity,
        pnl=final_equity - base_equity,
        total_return=final_equity / base_equity - 1.0 if base_equity > 0 else 0.0,
        annualized_return=stats.annualized_return,
        annualized_volatility=stats.annualized_volatility,
        sharpe=stats.sharpe,
        sortino=stats.sortino,
        max_drawdown=max_dd,
        max_drawdown_peak_ts=dd_peak_ts,
        max_drawdown_trough_ts=dd_trough_ts,
        traded_notional=traded,
        turnover=traded / mean_equity if mean_equity > 0 else None,
        fees=fees,
        fill_count=count,
        shortfall=shortfall,
        shortfall_bps=shortfall / decision_notional * 10_000 if decision_notional else None,
        min_cash=min((s.cash for s in window), default=base_equity),
    )
