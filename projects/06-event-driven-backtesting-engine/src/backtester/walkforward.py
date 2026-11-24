"""Walk-forward evaluation with strict train/test separation.

For each fold, every parameter combination is run on the training window only. The
combination with the best training objective is then run once on the following test
window, which it has never seen. Only those test-window results count as out-of-sample
evidence; the training results are reported next to them so the decay is visible.

Separation is enforced by construction: a training run is handed a dataset sliced to
events available before the test window starts, so there is nothing later for it to
read. A test run is handed the training window as warm-up history (it may observe but
not trade) plus the test window, and its metrics are measured over the test window only.
Each test run starts flat with the full initial cash; positions do not carry over between
folds.
"""

from __future__ import annotations

from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, replace
from itertools import product
from typing import Any

from .config import BacktestConfig, ConfigError
from .data import Dataset, DataSummary
from .engine import run_backtest
from .metrics import Metrics, ReturnStats, daily_returns, return_stats
from .results import BacktestResult
from .strategy import Strategy
from .timeutil import NS_PER_DAY

OBJECTIVES = ("sharpe", "sortino", "total_return")


@dataclass(frozen=True)
class WalkForwardConfig:
    train_ns: int
    test_ns: int
    param_grid: Mapping[str, Sequence[Any]]
    step_ns: int | None = None  # defaults to test_ns: back-to-back test windows
    anchored: bool = False  # True: training always starts at the first data point
    objective: str = "sharpe"

    def __post_init__(self) -> None:
        if self.train_ns <= 0 or self.test_ns <= 0:
            raise ConfigError("train and test windows must be positive")
        if self.step_ns is not None and self.step_ns < self.test_ns:
            raise ConfigError("step must be at least the test window, or test windows overlap")
        if self.objective not in OBJECTIVES:
            raise ConfigError(f"objective must be one of {OBJECTIVES}")
        if not self.param_grid or any(len(v) == 0 for v in self.param_grid.values()):
            raise ConfigError("param_grid needs at least one parameter with at least one value")


@dataclass(frozen=True)
class Candidate:
    params: Mapping[str, Any]
    metrics: Metrics
    objective: float | None


@dataclass(frozen=True)
class Fold:
    index: int
    train_start: int
    train_end: int
    test_start: int
    test_end: int
    candidates: tuple[Candidate, ...]
    selected: Candidate
    test_result: BacktestResult
    test_objective: float | None

    @property
    def test_metrics(self) -> Metrics:
        return self.test_result.metrics


@dataclass(frozen=True)
class WalkForwardResult:
    config: WalkForwardConfig
    base_config: BacktestConfig
    data_version: str
    data_summary: DataSummary
    folds: tuple[Fold, ...]
    combinations: int
    out_of_sample: ReturnStats  # every test window's daily returns, chained in order

    @property
    def backtests_run(self) -> int:
        return len(self.folds) * (self.combinations + 1)


def param_combinations(grid: Mapping[str, Sequence[Any]]) -> list[dict[str, Any]]:
    """Cartesian product in a fixed order: keys sorted, values as given."""
    keys = sorted(grid)
    return [dict(zip(keys, values, strict=True)) for values in product(*(grid[k] for k in keys))]


def plan_folds(dataset: Dataset, config: WalkForwardConfig) -> list[tuple[int, int, int, int]]:
    """(train_start, train_end, test_start, test_end) per fold; test_start == train_end.

    Windows are aligned to UTC midnight of the first day of data. Only complete test
    windows are used, so the tail of the data may be left out.
    """
    if dataset.first_ts is None or dataset.last_ts is None:
        raise ConfigError("cannot walk forward over an empty dataset")
    origin = dataset.first_ts - dataset.first_ts % NS_PER_DAY
    step = config.step_ns or config.test_ns
    folds = []
    index = 0
    while True:
        test_start = origin + config.train_ns + index * step
        test_end = test_start + config.test_ns
        if test_end > dataset.last_ts + 1:
            break
        train_start = origin if config.anchored else test_start - config.train_ns
        folds.append((train_start, test_start, test_start, test_end))
        index += 1
    if not folds:
        raise ConfigError(
            "the data is too short for even one fold: it must cover the training window "
            "plus one full test window"
        )
    return folds


def walk_forward(
    dataset: Dataset,
    strategy_factory: Callable[[Mapping[str, Any]], Strategy],
    base_config: BacktestConfig,
    config: WalkForwardConfig,
    *,
    describe: Callable[[Mapping[str, Any]], Mapping[str, Any]] | None = None,
) -> WalkForwardResult:
    """Run the walk-forward. ``describe`` supplies the strategy identity for run hashes."""
    combos = param_combinations(config.param_grid)
    train_config = replace(base_config, trade_from=None, oos_start=None)
    folds: list[Fold] = []
    chained: list[float] = []

    for index, (train_start, train_end, test_start, test_end) in enumerate(
        plan_folds(dataset, config)
    ):
        train_data = dataset.slice(train_start, train_end)
        candidates = []
        for params in combos:
            result = run_backtest(
                train_data,
                strategy_factory(params),
                train_config,
                strategy_info=describe(params) if describe else None,
            )
            candidates.append(
                Candidate(params, result.metrics, getattr(result.metrics, config.objective))
            )
        selected = _select(candidates)

        test_result = run_backtest(
            dataset.slice(train_start, test_end),
            strategy_factory(selected.params),
            replace(base_config, trade_from=test_start, oos_start=None),
            strategy_info=describe(selected.params) if describe else None,
        )
        chained.extend(
            r
            for _, r in daily_returns(
                test_result.snapshots, base_config.initial_cash, start=test_start
            )
        )
        folds.append(
            Fold(
                index=index,
                train_start=train_start,
                train_end=train_end,
                test_start=test_start,
                test_end=test_end,
                candidates=tuple(candidates),
                selected=selected,
                test_result=test_result,
                test_objective=getattr(test_result.metrics, config.objective),
            )
        )

    return WalkForwardResult(
        config=config,
        base_config=base_config,
        data_version=dataset.data_version,
        data_summary=dataset.summary,
        folds=tuple(folds),
        combinations=len(combos),
        out_of_sample=return_stats(
            chained, base_config.periods_per_year, float(base_config.risk_free_rate)
        ),
    )


def _select(candidates: Sequence[Candidate]) -> Candidate:
    """Highest objective wins; an undefined objective loses; ties go to the earliest."""
    best = candidates[0]
    for candidate in candidates[1:]:
        if candidate.objective is None:
            continue
        if best.objective is None or candidate.objective > best.objective:
            best = candidate
    return best
