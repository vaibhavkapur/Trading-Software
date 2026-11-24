"""Measure engine throughput on this machine: ``make bench``.

Method
  * Data: synthetic one-minute bars (390 per session) for SYMBOLS symbols over DAYS
    weekdays, generated in memory, so file parsing is not part of the measurement.
  * Workloads: a strategy that never trades (queue and callback overhead only) and the
    momentum example with fees, slippage, latency and a participation cap (orders, fills
    and accounting on top).
  * An "event" is one entry taken off the queue. Each bar is two (venue time, then
    availability time); order requests and executions add more.
  * Each workload runs REPEATS times in one process. "loop" is the event loop alone:
    fastest, median and slowest repeat. "whole run" is the median repeat including what
    follows the loop (reconciliation, metrics, hashing every order, fill, ledger entry and
    equity row), which is what a user waits for. Dataset generation is not timed.
  * Peak memory is the process's maximum resident set size at the end, so it includes
    the dataset and the results of every repeat.

Numbers depend on the machine, the Python build and the strategy. Quote them only with
the machine details this script prints.
"""

from __future__ import annotations

import argparse
import os
import platform
import resource
import statistics
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "examples"))

from momentum import Momentum  # noqa: E402

from backtester import BacktestConfig, ExecutionConfig, Strategy, run_backtest  # noqa: E402
from backtester.fixtures import synthetic_dataset  # noqa: E402
from backtester.timeutil import NS_PER_MS  # noqa: E402


class Idle(Strategy):
    """Receives every event and does nothing."""


def peak_rss_mb() -> float:
    peak = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
    # macOS reports bytes, Linux reports kilobytes.
    return peak / (1024 * 1024) if sys.platform == "darwin" else peak / 1024


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--symbols", type=int, default=5)
    parser.add_argument("--days", type=int, default=250)
    parser.add_argument("--repeats", type=int, default=5)
    args = parser.parse_args()

    symbols = tuple(f"S{i:02d}" for i in range(args.symbols))
    dataset = synthetic_dataset(symbols, days=args.days, seed=11, bar_seconds=60)
    frictions = BacktestConfig(
        seed=1,
        execution=ExecutionConfig(
            fee_bps=1,
            slippage_bps=3,
            spread_bps=2,
            latency_ns=50 * NS_PER_MS,
            participation_rate="0.05",
        ),
    )
    workloads = [
        ("idle strategy, no orders", Idle, BacktestConfig()),
        ("momentum, lookback 60, with frictions", lambda: Momentum(lookback=60), frictions),
    ]

    print("Benchmark: synthetic data, single process, single thread")
    print(f"  machine   {platform.platform()}, {platform.machine()}, {os.cpu_count()} logical CPUs")
    print(f"  python    {platform.python_implementation()} {platform.python_version()}")
    print(
        f"  dataset   {len(dataset):,} one-minute bars: {args.symbols} symbols x {args.days} days"
    )
    print(f"  repeats   {args.repeats} per workload")
    print()
    print(f"  {'':<40}{'':>11}{'':>9}{'loop, events/s':>36}{'whole run':>24}")
    print(
        f"  {'workload':<40}{'events':>11}{'fills':>9}{'best':>12}{'median':>12}{'worst':>12}"
        f"{'seconds':>10}{'events/s':>14}"
    )
    for name, factory, config in workloads:
        rates = []
        totals = []
        hashes = set()
        for _ in range(args.repeats):
            result = run_backtest(dataset, factory(), config)
            rates.append(result.runtime.events_per_second)
            totals.append(result.runtime.total_seconds)
            hashes.add(result.result_hash)
        if len(hashes) != 1:
            raise SystemExit(f"{name}: repeats produced different results")
        total = statistics.median(totals)
        print(
            f"  {name:<40}{result.runtime.events:>11,}{len(result.fills):>9,}"
            f"{max(rates):>12,.0f}{statistics.median(rates):>12,.0f}{min(rates):>12,.0f}"
            f"{total:>10.2f}{result.runtime.events / total:>14,.0f}"
        )
    print()
    print("  every repeat of a workload produced an identical result hash")
    print(f"  peak resident memory {peak_rss_mb():,.0f} MB (dataset plus all repeats)")


if __name__ == "__main__":
    main()
