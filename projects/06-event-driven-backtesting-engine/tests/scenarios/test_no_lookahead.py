"""No feature can read data timestamped later than simulated time.

The strongest evidence is behavioural: if nothing before time T depends on anything
after T, then deleting or rewriting the future must leave the past exactly as it was.
These tests do that to the whole pipeline (strategy inputs, risk decisions, fills,
accounting), with every stochastic and friction feature switched on.
"""

import pytest
from helpers import DAY, HOUR, Recorder, daily_bar, dataset
from mean_reversion import MeanReversion
from momentum import Momentum

from backtester import (
    Backtest,
    BacktestConfig,
    Bar,
    Dataset,
    EventType,
    ExecutionConfig,
    LookaheadError,
    MarketEvent,
    RiskLimits,
    Strategy,
    run_backtest,
)
from backtester.fixtures import demo_dataset, synthetic_dataset
from backtester.timeutil import NS_PER_MS

FRICTIONS = BacktestConfig(
    seed=11,
    execution=ExecutionConfig(
        fee_bps=1,
        slippage_bps=3,
        spread_bps=2,
        latency_ns=50 * NS_PER_MS,
        latency_jitter_ns=20 * NS_PER_MS,
        participation_rate="0.002",
        touch_fill_probability="0.5",
    ),
    risk=RiskLimits(allow_short=True, allow_leverage=True),
)
STRATEGIES = {
    "momentum": lambda: Momentum(lookback=15),
    "mean_reversion": lambda: MeanReversion(window=10, entry_bps=150),
}


class Observer(Strategy):
    """Wraps a strategy and records everything the context shows it at each event."""

    def __init__(self, inner: Strategy) -> None:
        self.inner = inner
        self.seen: list[tuple] = []

    def on_market_event(self, event, context):
        self.seen.append(
            (
                context.now,
                event,
                context.cash,
                context.equity,
                tuple(context.mark(s) for s in context.symbols),
                tuple(context.position(s) for s in context.symbols),
                context.history(event.symbol, 5),
                context.open_orders(),
            )
        )
        self.inner.on_market_event(event, context)

    def on_fill(self, fill, context):
        self.seen.append((context.now, fill, context.cash, context.equity))
        self.inner.on_fill(fill, context)

    def on_order_update(self, order, context):
        self.seen.append((context.now, order))
        self.inner.on_order_update(order, context)


def past(result, observer: Observer, cutoff: int) -> dict:
    """Everything a run produced, or showed its strategy, up to and including ``cutoff``."""
    return {
        "observations": [item for item in observer.seen if item[0] <= cutoff],
        "orders_sent": [
            (
                o.id,
                o.symbol,
                o.side,
                o.type,
                o.qty,
                o.limit_px,
                o.tif,
                o.submitted_at,
                o.decision_px,
            )
            for o in result.orders
            if o.submitted_at <= cutoff
        ],
        "rejections": [
            (o.id, o.reason)
            for o in result.orders
            if o.submitted_at <= cutoff and o.reason and o.closed_at <= cutoff
        ],
        "fills": [f for f in result.fills if f.ts <= cutoff],
        "ledger": [e for e in result.ledger if e.ts <= cutoff],
        "equity": [s for s in result.snapshots if s.ts <= cutoff],
    }


def run_observed(data: Dataset, name: str):
    observer = Observer(STRATEGIES[name]())
    return run_backtest(data, observer, FRICTIONS), observer


def rewrite_future(data: Dataset, cutoff: int) -> Dataset:
    """Triple every price and halve every volume in bars that open after ``cutoff``."""
    events = []
    for event in data.events:
        bar = event.payload
        if bar.ts_open > cutoff:
            bar = Bar(
                bar.ts_open,
                bar.ts_close,
                bar.open * 3,
                bar.high * 3,
                bar.low * 3,
                bar.close * 3,
                bar.volume // 2,
            )
            event = MarketEvent.bar(event.symbol, bar, event.ts)
        events.append(event)
    return Dataset.build(events, data.corporate_actions, price_decimals=data.price_decimals)


@pytest.fixture(scope="module")
def data() -> Dataset:
    return demo_dataset(seed=3, days=330)  # includes the split, the dividend and the gap


@pytest.mark.parametrize("name", sorted(STRATEGIES))
@pytest.mark.parametrize("fraction", [0.2, 0.5, 0.8, 0.95])
def test_deleting_the_future_does_not_change_the_past(data, name, fraction):
    cutoff = data.events[int(len(data) * fraction)].ts
    full, full_observer = run_observed(data, name)
    truncated, truncated_observer = run_observed(data.slice(end=cutoff + 1), name)

    before = past(full, full_observer, cutoff)
    assert before == past(truncated, truncated_observer, cutoff)
    assert before["fills"] and before["observations"]  # the comparison is not vacuous
    assert len(full.fills) > len(before["fills"])  # and there was a future to delete


@pytest.mark.parametrize("name", sorted(STRATEGIES))
@pytest.mark.parametrize("fraction", [0.3, 0.6, 0.9])
def test_rewriting_the_future_does_not_change_the_past(data, name, fraction):
    cutoff = data.events[int(len(data) * fraction)].ts
    original, original_observer = run_observed(data, name)
    altered, altered_observer = run_observed(rewrite_future(data, cutoff), name)

    assert past(original, original_observer, cutoff) == past(altered, altered_observer, cutoff)
    assert original.result_hash != altered.result_hash  # the future really was different


def test_intraday_data_with_latency_passes_the_same_test():
    data = synthetic_dataset(("AAA", "BBB"), days=6, seed=5, bar_seconds=300)
    cutoff = data.events[len(data) // 2].ts
    full, full_observer = run_observed(data, "momentum")
    truncated, truncated_observer = run_observed(data.slice(end=cutoff + 1), "momentum")
    before = past(full, full_observer, cutoff)
    assert before == past(truncated, truncated_observer, cutoff)
    assert before["fills"]


def test_the_strategy_never_holds_a_bar_before_its_publication_time():
    # Each symbol publishes with a different delay, some of them longer than a whole bar.
    delays = {"FAST": 0, "SLOW": 3 * HOUR, "LATE": 2 * DAY + HOUR}
    events = [
        daily_bar(symbol, day, 100 + day, 102 + day, 99 + day, 101 + day, delay=delay)
        for symbol, delay in delays.items()
        for day in range(12)
    ]
    published = {(e.symbol, e.payload): e.ts for e in events}
    checked = []

    class Auditor(Strategy):
        def on_market_event(self, event, context):
            assert event.ts == context.now
            assert event.ts_event <= context.now
            for symbol in context.symbols:
                for bar in context.history(symbol, adjusted=False):
                    assert published[(symbol, bar)] <= context.now
                    assert bar.ts_close <= context.now
                    checked.append(bar)
                mark = context.mark(symbol)
                history = context.history(symbol)
                assert mark is None or mark == history[-1].close

    run_backtest(dataset(events), Auditor(), BacktestConfig())
    assert len(checked) > 100


def test_a_late_bar_is_seen_late_and_cannot_be_traded_retroactively():
    late = 2 * DAY + HOUR
    events = [daily_bar("X", d, 100 + d, 101 + d, 99 + d, 100 + d, delay=late) for d in range(6)]
    strategy = Recorder({0: lambda ctx: ctx.submit_order("X", "buy", 10)})
    result = run_backtest(dataset(events), strategy, BacktestConfig())

    first_seen = strategy.log[0][1]
    assert first_seen == events[0].ts  # bar 0 arrives after bars 1 and 2 have closed
    (fill,) = result.fills
    # Bars 1 and 2 opened before the order existed; the first bar it can trade is bar 3.
    assert fill.px == events[3].payload.open
    assert fill.ts == events[3].payload.ts_close


def test_the_engine_refuses_to_deliver_an_event_early():
    event = daily_bar("X", 5, 100, 101, 99, 100)
    backtest = Backtest(dataset([daily_bar("X", 0, 100, 101, 99, 100)]), Strategy())
    with pytest.raises(LookaheadError):
        backtest._on_market_data(event)  # the clock is still at day 0


def test_the_context_offers_no_way_to_ask_about_the_future():
    data = dataset([daily_bar("X", d, 100, 101, 99, 100) for d in range(5)])
    sizes = []

    class Counter(Strategy):
        def on_market_event(self, event, context):
            sizes.append((len(context.history("X")), len(context.history("X", 100))))
            assert not hasattr(context, "dataset") and not hasattr(context, "events")

    run_backtest(data, Counter(), BacktestConfig())
    assert sizes == [(n, n) for n in range(1, 6)]
    assert all(e.event_type is EventType.BAR for e in data.events)
