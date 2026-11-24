import pytest
from helpers import DAY, T0, dataset, flat_bars

from backtester import ConfigError, WalkForwardConfig
from backtester.metrics import Metrics
from backtester.walkforward import Candidate, _select, param_combinations, plan_folds


def wf(**kwargs) -> WalkForwardConfig:
    defaults = {"train_ns": 10 * DAY, "test_ns": 5 * DAY, "param_grid": {"a": [1]}}
    return WalkForwardConfig(**{**defaults, **kwargs})


def candidate(name: str, objective: float | None) -> Candidate:
    metrics = Metrics(
        None, None, 0, 0, 0, 0, 0.0, None, None, objective, None, 0.0, None, None, 0, None, 0, 0, 0,
        None, 0,
    )  # fmt: skip
    return Candidate({"name": name}, metrics, objective)


def test_rolling_folds_are_back_to_back_and_test_starts_where_train_ends():
    folds = plan_folds(dataset(flat_bars("X", 26)), wf())
    assert folds == [
        (T0, T0 + 10 * DAY, T0 + 10 * DAY, T0 + 15 * DAY),
        (T0 + 5 * DAY, T0 + 15 * DAY, T0 + 15 * DAY, T0 + 20 * DAY),
        (T0 + 10 * DAY, T0 + 20 * DAY, T0 + 20 * DAY, T0 + 25 * DAY),
    ]
    for train_start, train_end, test_start, test_end in folds:
        assert train_start < train_end == test_start < test_end


def test_anchored_folds_always_train_from_the_start():
    folds = plan_folds(dataset(flat_bars("X", 26)), wf(anchored=True))
    assert [f[0] for f in folds] == [T0, T0, T0]
    assert [f[1] for f in folds] == [T0 + 10 * DAY, T0 + 15 * DAY, T0 + 20 * DAY]


def test_only_complete_test_windows_are_used():
    assert len(plan_folds(dataset(flat_bars("X", 20)), wf())) == 1  # data ends on day 19
    assert len(plan_folds(dataset(flat_bars("X", 21)), wf())) == 2


def test_a_step_larger_than_the_test_window_leaves_gaps_between_folds():
    folds = plan_folds(dataset(flat_bars("X", 40)), wf(step_ns=10 * DAY))
    assert [f[2] for f in folds] == [T0 + 10 * DAY, T0 + 20 * DAY, T0 + 30 * DAY]


def test_data_too_short_for_one_fold_is_an_error():
    with pytest.raises(ConfigError, match="too short"):
        plan_folds(dataset(flat_bars("X", 12)), wf())
    with pytest.raises(ConfigError, match="empty"):
        plan_folds(dataset([]), wf())


@pytest.mark.parametrize(
    "kwargs",
    [
        {"train_ns": 0},
        {"test_ns": -1},
        {"step_ns": 4 * DAY},  # shorter than the test window: test windows would overlap
        {"objective": "profit"},
        {"param_grid": {}},
        {"param_grid": {"a": []}},
    ],
)
def test_invalid_walk_forward_settings_are_refused(kwargs):
    with pytest.raises(ConfigError):
        wf(**kwargs)


def test_parameter_combinations_have_a_fixed_order():
    grid = {"b": [1, 2], "a": ["x", "y"]}
    assert param_combinations(grid) == [
        {"a": "x", "b": 1},
        {"a": "x", "b": 2},
        {"a": "y", "b": 1},
        {"a": "y", "b": 2},
    ]
    assert param_combinations(dict(reversed(grid.items()))) == param_combinations(grid)


def test_selection_takes_the_best_and_breaks_ties_by_order():
    picks = [candidate("a", 1.0), candidate("b", 2.0), candidate("c", 2.0), candidate("d", None)]
    assert _select(picks).params == {"name": "b"}


def test_an_undefined_objective_never_beats_a_defined_one():
    assert _select([candidate("a", None), candidate("b", -5.0)]).params == {"name": "b"}
    assert _select([candidate("a", None), candidate("b", None)]).params == {"name": "a"}
