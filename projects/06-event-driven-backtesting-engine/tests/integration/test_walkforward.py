"""Walk-forward evaluation keeps training and test data apart."""

import pytest
from helpers import DAY
from momentum import Momentum

from backtester import (
    BacktestConfig,
    Bar,
    Dataset,
    ExecutionConfig,
    MarketEvent,
    OrderStatus,
    Strategy,
    WalkForwardConfig,
    walk_forward,
)
from backtester.fixtures import demo_dataset
from backtester.metrics import daily_returns, return_stats
from backtester.walkforward import plan_folds

BASE = BacktestConfig(seed=3, execution=ExecutionConfig(fee_bps=1, slippage_bps=3))
PLAN = WalkForwardConfig(train_ns=120 * DAY, test_ns=60 * DAY, param_grid={"lookback": [5, 15, 40]})


@pytest.fixture(scope="module")
def data():
    return demo_dataset(seed=7, days=330)


class Spy(Strategy):
    """Momentum, plus a record of when it was shown data and when it tried to trade."""

    instances: list["Spy"] = []

    def __init__(self, **params):
        self.inner = Momentum(**params)
        self.params = params
        self.seen: list[int] = []
        self.history_ends: list[int] = []
        Spy.instances.append(self)

    def on_market_event(self, event, context):
        self.seen.append(event.ts)
        self.history_ends.extend(
            bar.ts_close for symbol in context.symbols for bar in context.history(symbol)[-1:]
        )
        self.inner.on_market_event(event, context)

    def on_fill(self, fill, context):
        self.inner.on_fill(fill, context)


@pytest.fixture(scope="module")
def outcome(data):
    Spy.instances = []
    result = walk_forward(data, lambda params: Spy(**params), BASE, PLAN)
    return result, list(Spy.instances)


def test_training_runs_are_never_shown_data_from_their_test_window(outcome):
    result, spies = outcome
    per_fold = len(PLAN.param_grid["lookback"]) + 1
    assert len(spies) == per_fold * len(result.folds) and len(result.folds) >= 3
    for fold in result.folds:
        group = spies[fold.index * per_fold : (fold.index + 1) * per_fold]
        *training, test = group
        assert [spy.params["lookback"] for spy in training] == [5, 15, 40]
        for spy in training:
            assert spy.seen and fold.train_start <= min(spy.seen)
            assert max(spy.seen) < fold.test_start
            assert max(spy.history_ends) < fold.test_start
        assert test.params == dict(fold.selected.params)
        assert fold.test_start <= max(test.seen) < fold.test_end


def test_test_runs_warm_up_on_training_data_but_trade_only_in_the_test_window(outcome):
    result, _ = outcome
    for fold in result.folds:
        run = fold.test_result
        assert run.config.trade_from == fold.test_start
        assert run.fills, "the fold should trade, or the check below proves nothing"
        assert all(fold.test_start <= fill.ts < fold.test_end for fill in run.fills)
        early = [o for o in run.orders if o.submitted_at < fold.test_start]
        assert early, "the strategy should have tried to trade during warm-up"
        assert all(o.status is OrderStatus.REJECTED and o.reason == "WARMUP" for o in early)
        # The books start flat with full cash at the start of the test window.
        before = [s for s in run.snapshots if s.ts < fold.test_start]
        assert all(s.equity == BASE.initial_cash and not s.holdings for s in before)
        assert fold.test_start <= fold.test_metrics.start_ts <= run.fills[0].ts
        assert fold.test_metrics.initial_equity == BASE.initial_cash


def test_selection_uses_training_results_only(outcome):
    result, _ = outcome
    for fold in result.folds:
        scores = [c.objective for c in fold.candidates]
        defined = [s for s in scores if s is not None]
        assert fold.selected in fold.candidates
        if defined:
            assert fold.selected.objective == max(defined)
            assert fold.selected is fold.candidates[scores.index(max(defined))]  # earliest wins
    chosen = {fold.selected.params["lookback"] for fold in result.folds}
    assert chosen <= {5, 15, 40}


def test_changing_the_test_window_cannot_change_what_was_selected(data, outcome):
    """Rewrite all data from the last fold's test window on; its selection must not move."""
    result, _ = outcome
    last = result.folds[-1]
    events = []
    for event in data.events:
        if event.ts >= last.test_start:
            bar = event.payload
            flipped = Bar(
                bar.ts_open, bar.ts_close, bar.open * 2, bar.high * 2, bar.low * 2,
                bar.close * 2, bar.volume,
            )  # fmt: skip
            event = MarketEvent.bar(event.symbol, flipped, event.ts)
        events.append(event)
    altered = walk_forward(
        Dataset.build(events, data.corporate_actions), lambda p: Momentum(**p), BASE, PLAN
    )
    changed = altered.folds[-1]
    assert [c.objective for c in changed.candidates] == [c.objective for c in last.candidates]
    assert [c.metrics for c in changed.candidates] == [c.metrics for c in last.candidates]
    assert changed.selected.params == last.selected.params
    assert changed.test_result.result_hash != last.test_result.result_hash
    # Earlier folds lie wholly before the change and are untouched.
    for a, b in zip(altered.folds[:-1], result.folds[:-1], strict=True):
        assert a.test_result.result_hash == b.test_result.result_hash


def test_folds_tile_the_data_without_overlap(data, outcome):
    result, _ = outcome
    planned = plan_folds(data, PLAN)
    assert [(f.train_start, f.train_end, f.test_start, f.test_end) for f in result.folds] == planned
    for before, after in zip(result.folds, result.folds[1:], strict=False):
        assert before.test_end == after.test_start
    assert result.folds[-1].test_end <= data.last_ts + 1
    assert all(f.train_end - f.train_start == PLAN.train_ns for f in result.folds)


def test_the_chained_out_of_sample_series_is_exactly_the_test_windows(outcome):
    result, _ = outcome
    chained = []
    for fold in result.folds:
        chained += [
            r for _, r in daily_returns(
                fold.test_result.snapshots, BASE.initial_cash, start=fold.test_start
            )
        ]  # fmt: skip
    expected = return_stats(chained, BASE.periods_per_year, 0.0)
    assert result.out_of_sample == expected
    assert result.out_of_sample.periods == len(chained) > 100
    growth = 1.0
    for fold in result.folds:
        growth *= 1 + fold.test_metrics.total_return
    assert result.out_of_sample.total_return == pytest.approx(growth - 1, abs=1e-12)


def test_walk_forward_is_deterministic(data, outcome):
    result, _ = outcome
    again = walk_forward(data, lambda params: Momentum(**params), BASE, PLAN)
    assert [f.selected.params for f in again.folds] == [f.selected.params for f in result.folds]
    assert [f.test_result.result_hash for f in again.folds] == [
        f.test_result.result_hash for f in result.folds
    ]
    assert again.out_of_sample == result.out_of_sample


def test_anchored_training_grows_from_the_start_of_the_data(data):
    anchored = WalkForwardConfig(
        train_ns=120 * DAY, test_ns=60 * DAY, param_grid={"lookback": [10]}, anchored=True
    )
    result = walk_forward(data, lambda params: Momentum(**params), BASE, anchored)
    starts = {fold.train_start for fold in result.folds}
    assert len(starts) == 1
    lengths = [fold.train_end - fold.train_start for fold in result.folds]
    assert lengths == sorted(lengths) and lengths[0] == 120 * DAY and lengths[-1] > lengths[0]
