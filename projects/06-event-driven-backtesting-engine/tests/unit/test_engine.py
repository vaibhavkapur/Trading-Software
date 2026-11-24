"""Engine behaviour on small hand-built datasets where every number can be checked by eye."""

import pytest
from helpers import (
    DAY,
    DECIMALS,
    HOUR,
    MINUTE,
    T0,
    Recorder,
    daily_bar,
    dataset,
    day_close,
    day_open,
    flat_bars,
    minute_bar,
    px,
    quote,
)

from backtester import (
    Backtest,
    BacktestConfig,
    ConfigError,
    CorporateAction,
    ExecutionConfig,
    OrderStatus,
    RiskLimits,
    Strategy,
    run_backtest,
)
from backtester.report import render_report
from backtester.results import orders_csv
from backtester.timeutil import NS_PER_SEC

CASH = px(100_000)


def config(**kwargs) -> BacktestConfig:
    execution = ExecutionConfig(**kwargs.pop("execution", {}))
    risk = RiskLimits(**kwargs.pop("risk", {}))
    return BacktestConfig(initial_cash=CASH, execution=execution, risk=risk, **kwargs)


def rising_bars(days=5) -> list:
    """Day d opens at 100 + 2d and closes one higher."""
    return [
        daily_bar("X", d, 100 + 2 * d, 102 + 2 * d, 99 + 2 * d, 101 + 2 * d) for d in range(days)
    ]


# -- causality --------------------------------------------------------------------------


def test_a_market_order_fills_at_the_next_bars_open_never_on_the_bar_that_triggered_it():
    strategy = Recorder({0: lambda ctx: ctx.submit_order("X", "buy", 10)})
    result = run_backtest(dataset(rising_bars()), strategy, config())
    (fill,) = result.fills
    assert fill.px == px(102)  # day 1's open, not day 0's close of 101
    assert fill.ts == day_close(1)
    assert result.orders[0].decision_px == px(101)


def test_the_strategy_hears_about_a_fill_before_the_bar_that_produced_it():
    strategy = Recorder({0: lambda ctx: ctx.submit_order("X", "buy", 10)})
    run_backtest(dataset(rising_bars(3)), strategy, config())
    assert strategy.log == [
        ("data", day_close(0), "X"),
        ("fill", day_close(1), "O0000001"),
        ("data", day_close(1), "X"),
        ("data", day_close(2), "X"),
    ]


def test_the_portfolio_already_reflects_a_fill_when_on_fill_is_called():
    seen = {}

    class Check(Strategy):
        def on_market_event(self, event, context):
            if not context.open_orders() and context.position("X") == 0:
                context.submit_order("X", "buy", 10)

        def on_fill(self, fill, context):
            seen["position"] = context.position("X")
            seen["cash"] = context.cash
            seen["status"] = context.order(fill.order_id).status

    run_backtest(dataset(rising_bars(3)), Check(), config())
    assert seen == {"position": 10, "cash": CASH - 10 * px(102), "status": OrderStatus.FILLED}


def test_events_are_delivered_exactly_at_their_availability_time():
    events = [daily_bar("X", d, 100, 101, 99, 100, delay=2 * HOUR) for d in range(3)]
    strategy = Recorder()
    run_backtest(dataset(events), strategy, config())
    assert [when for _, when, _ in strategy.log] == [day_close(d) + 2 * HOUR for d in range(3)]


def test_with_delayed_data_the_fill_is_known_before_the_bar_that_caused_it_is_published():
    events = [
        daily_bar("X", d, 100 + d, 101 + d, 99 + d, 100 + d, delay=2 * HOUR) for d in range(3)
    ]
    strategy = Recorder({0: lambda ctx: ctx.submit_order("X", "buy", 10)})
    result = run_backtest(dataset(events), strategy, config())
    assert result.fills[0].ts == day_close(1)
    assert strategy.log[:3] == [
        ("data", day_close(0) + 2 * HOUR, "X"),
        ("fill", day_close(1), "O0000001"),
        ("data", day_close(1) + 2 * HOUR, "X"),
    ]


def test_same_timestamp_events_reach_the_strategy_in_symbol_order():
    events = flat_bars("B", 2) + flat_bars("A", 2) + flat_bars("C", 2)
    strategy = Recorder()
    run_backtest(dataset(events), strategy, config())
    assert [symbol for _, _, symbol in strategy.log] == ["A", "B", "C", "A", "B", "C"]


# -- latency ----------------------------------------------------------------------------


def minute_bars(count=5) -> list:
    return [minute_bar("X", i, 100 + i, 101 + i, 99 + i, 100 + i) for i in range(count)]


def test_without_latency_an_order_reaches_the_exchange_in_time_for_the_next_bar():
    strategy = Recorder({0: lambda ctx: ctx.submit_order("X", "buy", 10)})
    result = run_backtest(dataset(minute_bars()), strategy, config())
    assert result.fills[0].px == px(101)  # bar 1 opens the instant bar 0 closes
    assert result.orders[0].arrived_at == T0 + MINUTE


def test_latency_makes_the_order_miss_the_bar_that_has_already_opened():
    strategy = Recorder({0: lambda ctx: ctx.submit_order("X", "buy", 10)})
    slow = config(execution={"latency_ns": NS_PER_SEC})
    result = run_backtest(dataset(minute_bars()), strategy, slow)
    assert result.orders[0].arrived_at == T0 + MINUTE + NS_PER_SEC
    assert result.fills[0].px == px(102)  # bar 2's open
    assert result.fills[0].ts == T0 + 3 * MINUTE


def test_latency_jitter_is_seeded_and_bounded():
    def arrivals(seed: int) -> list[int]:
        script = {i: (lambda ctx: ctx.submit_order("X", "buy", 1)) for i in range(20)}
        cfg = config(seed=seed, execution={"latency_ns": 1_000, "latency_jitter_ns": 500})
        result = run_backtest(dataset(minute_bars(25)), Recorder(script), cfg)
        return [o.arrived_at - o.submitted_at for o in result.orders]

    first = arrivals(1)
    assert first == arrivals(1)
    assert first != arrivals(2)
    assert all(1_000 <= delay <= 1_500 for delay in first) and len(set(first)) > 1


# -- cancels ----------------------------------------------------------------------------


def test_cancel_reaches_a_resting_order_and_the_strategy_is_told():
    def enter(ctx):
        ctx.submit_order("X", "buy", 10, "limit", px(50), tag="deep")

    def cancel(ctx):
        (order,) = ctx.open_orders("X")
        assert ctx.cancel_order(order.id) is True
        assert ctx.cancel_order(order.id) is True  # asking twice is harmless

    strategy = Recorder({0: enter, 1: cancel})
    result = run_backtest(dataset(flat_bars("X", 4)), strategy, config())
    (order,) = result.orders
    assert (order.status, order.reason, order.tag) == (OrderStatus.CANCELED, "USER_CANCEL", "deep")
    assert order.closed_at == day_close(1)
    assert [u.status for u in strategy.updates] == [OrderStatus.CANCELED]
    assert result.fills == ()


def test_cancel_of_an_unknown_or_finished_order_is_refused_locally():
    outcomes = []

    def enter(ctx):
        outcomes.append(ctx.cancel_order("O9999999"))
        ctx.submit_order("X", "buy", 10)

    def late(ctx):
        outcomes.append(ctx.cancel_order("O0000001"))  # filled on this bar already

    run_backtest(dataset(flat_bars("X", 3)), Recorder({0: enter, 1: late}), config())
    assert outcomes == [False, False]


def test_a_cancel_in_flight_when_the_order_fills_loses_the_race():
    class Racer(Strategy):
        def on_market_event(self, event, context):
            if context.now == day_close(0):
                context.submit_order("X", "buy", 10)
                context.set_timer(day_close(1) - 30 * MINUTE, "cancel")

        def on_timer(self, timer, context):
            (order,) = context.open_orders()
            assert context.cancel_order(order.id) is True

    slow = config(execution={"latency_ns": HOUR})
    result = run_backtest(dataset(flat_bars("X", 3)), Racer(), slow)
    assert result.orders[0].status is OrderStatus.FILLED
    assert result.late_cancels == 1


# -- rejects ----------------------------------------------------------------------------


def test_a_risk_reject_is_reported_at_once_and_never_reaches_the_exchange():
    strategy = Recorder({0: lambda ctx: ctx.submit_order("X", "sell", 10)})
    result = run_backtest(dataset(flat_bars("X", 3)), strategy, config())
    (order,) = result.orders
    assert (order.status, order.reason) == (OrderStatus.REJECTED, "SHORT_NOT_ALLOWED")
    assert order.arrived_at is None and order.closed_at == day_close(0)
    assert strategy.log[1] == ("update", day_close(0), order.id, "rejected", "SHORT_NOT_ALLOWED")
    assert result.reject_reasons == {"SHORT_NOT_ALLOWED": 1}


def test_invalid_orders_are_rejected_not_raised():
    def bad(ctx):
        ctx.submit_order("X", "buy", 0)
        ctx.submit_order("X", "buy", 5, "limit")
        ctx.submit_order("NOPE", "buy", 5)

    result = run_backtest(dataset(flat_bars("X", 2)), Recorder({0: bad}), config())
    assert [o.reason for o in result.orders] == [
        "INVALID_QTY",
        "INVALID_LIMIT_PRICE",
        "NO_MARKET_DATA",
    ]


def test_malformed_orders_do_not_break_the_report():
    def bad(ctx):
        ctx.submit_order("X", "buy", None)
        ctx.submit_order("X", "buy", 2.5)
        ctx.submit_order("X", "buy", 5, "limit", 99.5)

    result = run_backtest(dataset(flat_bars("X", 2)), Recorder({0: bad}), config())
    assert [o.status for o in result.orders] == [OrderStatus.REJECTED] * 3
    rows = orders_csv(result.orders, DECIMALS).splitlines()
    assert len(rows) == 4 and ",99.5," in rows[3]
    assert "INVALID_LIMIT_PRICE: 1" in render_report(result)


def test_an_unknown_side_is_a_programming_error():
    strategy = Recorder({0: lambda ctx: ctx.submit_order("X", "hold", 1)})
    with pytest.raises(ValueError, match="hold") as info:
        run_backtest(dataset(flat_bars("X", 2)), strategy, config())
    assert any("simulated time" in note for note in info.value.__notes__)


def test_no_trading_during_warm_up():
    script = {i: (lambda ctx: ctx.submit_order("X", "buy", 1)) for i in range(4)}
    warm = config(trade_from=day_close(2))
    result = run_backtest(dataset(flat_bars("X", 5)), Recorder(script), warm)
    assert [o.reason for o in result.orders] == ["WARMUP", "WARMUP", None, None]
    assert result.metrics.fill_count == 2
    assert result.metrics.start_ts == day_close(2)


def test_orders_sent_after_the_last_event_are_refused():
    class Late(Strategy):
        def on_end(self, context):
            self.order_id = context.submit_order("X", "buy", 1)
            self.cancel = context.cancel_order(self.order_id)

    strategy = Late()
    result = run_backtest(dataset(flat_bars("X", 2)), strategy, config())
    assert result.orders[0].reason == "RUN_ENDED" and strategy.cancel is False


def test_an_order_still_working_at_the_end_is_reported_as_open():
    strategy = Recorder({0: lambda ctx: ctx.submit_order("X", "buy", 10, "limit", px(50))})
    result = run_backtest(dataset(flat_bars("X", 3)), strategy, config())
    assert result.orders[0].status is OrderStatus.WORKING
    assert result.order_status_counts["working"] == 1 and result.fill_ratio == 0.0


# -- partial fills through the whole stack ----------------------------------------------


def test_partial_fills_accumulate_on_the_order_and_in_the_portfolio():
    strategy = Recorder({0: lambda ctx: ctx.submit_order("X", "buy", 250)})
    capped = config(execution={"participation_rate": "0.1"})
    result = run_backtest(dataset(flat_bars("X", 5, volume=1_000)), strategy, capped)
    assert [f.qty for f in result.fills] == [100, 100, 50]
    assert result.orders[0].status is OrderStatus.FILLED
    assert [s.holdings[0][1] for s in result.snapshots[1:4]] == [100, 200, 250]


# -- timers -----------------------------------------------------------------------------


def test_timers_fire_in_time_order_after_everything_else_at_that_instant():
    class Timed(Strategy):
        def __init__(self):
            self.log = []

        def on_start(self, context):
            context.set_timer(day_close(1), "at-close")
            context.set_timer(day_open(1), "at-open")
            context.set_timer(day_close(9), "after-the-data-ends")

        def on_market_event(self, event, context):
            self.log.append(("data", context.now))

        def on_timer(self, timer, context):
            self.log.append((timer.name, context.now))

    strategy = Timed()
    run_backtest(dataset(flat_bars("X", 3)), strategy, config())
    assert strategy.log == [
        ("data", day_close(0)),
        ("at-open", day_open(1)),
        ("data", day_close(1)),
        ("at-close", day_close(1)),
        ("data", day_close(2)),
    ]


def test_a_self_renewing_timer_stops_at_the_end_of_the_data():
    class Tick(Strategy):
        count = 0

        def on_start(self, context):
            context.set_timer(context.now, "tick")

        def on_timer(self, timer, context):
            self.count += 1
            context.set_timer(context.now + DAY, "tick")

    strategy = Tick()
    run_backtest(dataset(flat_bars("X", 4)), strategy, config())
    assert strategy.count == 4


def test_a_timer_in_the_past_is_an_error():
    strategy = Recorder({1: lambda ctx: ctx.set_timer(ctx.now - 1, "too-late")})
    with pytest.raises(ValueError, match="in the past"):
        run_backtest(dataset(flat_bars("X", 3)), strategy, config())


# -- corporate actions ------------------------------------------------------------------


def split_data():
    bars = [daily_bar("X", d, 100, 100, 100, 100) for d in range(3)]
    bars += [daily_bar("X", d, 50, 50, 50, 50) for d in range(3, 6)]
    return dataset(bars, [CorporateAction.split(T0 + 3 * DAY, "X", 2, 1)])


def test_a_split_doubles_the_position_and_leaves_equity_unchanged():
    strategy = Recorder({0: lambda ctx: ctx.submit_order("X", "buy", 10)})
    result = run_backtest(split_data(), strategy, config())
    assert result.snapshots[-1].holdings == (("X", 20, px(50)),)
    assert {s.equity for s in result.snapshots} == {CASH}
    assert [a.symbol for a in strategy.actions] == ["X"]


def test_history_is_restated_only_for_splits_that_have_already_happened():
    closes = {}

    def before(ctx):
        closes["before"] = [b.close for b in ctx.history("X")]

    def after(ctx):
        closes["adjusted"] = [b.close for b in ctx.history("X")]
        closes["raw"] = [b.close for b in ctx.history("X", adjusted=False)]
        closes["last_two"] = [b.close for b in ctx.history("X", 2)]
        closes["volume"] = [b.volume for b in ctx.history("X")]

    run_backtest(split_data(), Recorder({2: before, 3: after}), config())
    assert closes["before"] == [px(100)] * 3  # the split is still in the future here
    assert closes["adjusted"] == [px(50)] * 4
    assert closes["raw"] == [px(100)] * 3 + [px(50)]
    assert closes["last_two"] == [px(50)] * 2
    assert closes["volume"] == [20_000] * 3 + [10_000]


def test_a_split_cancels_open_orders_in_that_symbol():
    def enter(ctx):
        ctx.submit_order("X", "buy", 10, "limit", px(90))

    strategy = Recorder({0: enter})
    result = run_backtest(split_data(), strategy, config())
    (order,) = result.orders
    assert (order.status, order.reason) == (OrderStatus.CANCELED, "CORPORATE_ACTION")
    assert order.closed_at == T0 + 3 * DAY
    assert result.fills == ()  # the post-split price of 50 never met the stale limit of 90


def test_an_order_in_flight_across_a_split_is_canceled_on_arrival():
    strategy = Recorder({2: lambda ctx: ctx.submit_order("X", "buy", 10)})
    slow = config(execution={"latency_ns": 4 * HOUR})  # sent 21:00, arrives after midnight
    result = run_backtest(split_data(), strategy, slow)
    (order,) = result.orders
    assert (order.status, order.reason) == (OrderStatus.CANCELED, "CORPORATE_ACTION")


def test_a_dividend_is_credited_to_holders_on_the_action_date():
    events = flat_bars("X", 5)
    data = dataset(events, [CorporateAction.dividend(T0 + 3 * DAY, "X", px("0.50"))])
    strategy = Recorder({0: lambda ctx: ctx.submit_order("X", "buy", 10)})
    result = run_backtest(data, strategy, config())
    assert result.reconciliation["dividends"] == px(5)
    assert result.snapshots[-1].equity == CASH + px(5)


# -- quotes -----------------------------------------------------------------------------


def test_quotes_drive_fills_at_venue_time_and_marks_at_receive_time():
    lag = 50_000_000  # the strategy sees each quote 50 ms after the exchange does
    events = [
        quote("X", T0 + 1 * NS_PER_SEC, 99.99, 100.01, delay=lag),
        quote("X", T0 + 2 * NS_PER_SEC, 100.09, 100.11, delay=lag),
        quote("X", T0 + 3 * NS_PER_SEC, 100.19, 100.21, delay=lag),
    ]
    seen = {}

    def act(ctx):
        seen["quote"] = ctx.last_quote("X")
        seen["mark"] = ctx.mark("X")
        ctx.submit_order("X", "buy", 10)

    result = run_backtest(dataset(events), Recorder({0: act}), config())
    assert seen["mark"] == px(100.00) and seen["quote"].ask == px(100.01)
    (fill,) = result.fills
    # Sent at 1.05 s with no order latency: the book still shows the first quote.
    assert (fill.px, fill.ts) == (px(100.01), T0 + NS_PER_SEC + lag)


# -- run hygiene ------------------------------------------------------------------------


def test_one_snapshot_per_timestamp_and_time_never_goes_back():
    events = flat_bars("A", 4) + flat_bars("B", 4)
    result = run_backtest(dataset(events), Recorder(), config())
    stamps = [s.ts for s in result.snapshots]
    assert stamps == [day_close(d) for d in range(4)]


def test_an_idle_strategy_ends_with_exactly_its_starting_cash():
    result = run_backtest(dataset(rising_bars()), Strategy(), config())
    assert result.metrics.final_equity == CASH and result.metrics.sharpe is None
    assert result.orders == () and result.fills == () and len(result.ledger) == 1


def test_an_empty_dataset_runs_and_reports_nothing():
    result = run_backtest(dataset([]), Strategy(), config())
    assert result.snapshots == () and result.metrics.final_equity == CASH
    assert result.runtime.events == 0


def test_a_backtest_cannot_be_run_twice():
    backtest = Backtest(dataset(flat_bars("X", 2)), Strategy(), config())
    backtest.run()
    with pytest.raises(RuntimeError, match="once"):
        backtest.run()


def test_dataset_and_config_must_agree_on_price_precision():
    with pytest.raises(ConfigError, match="decimals"):
        Backtest(dataset([]), Strategy(), BacktestConfig(price_decimals=DECIMALS - 2))


def test_the_strategy_rng_is_seeded_by_the_run():
    def draws(seed: int) -> list[int]:
        out = []
        run_backtest(
            dataset(flat_bars("X", 3)),
            Recorder({0: lambda ctx: out.extend(ctx.rng.randrange(10**6) for _ in range(3))}),
            config(seed=seed),
        )
        return out

    assert draws(5) == draws(5) != draws(6)


def test_config_round_trips_through_its_dictionary_form():
    original = config(
        seed=9,
        execution={"fee_bps": "1.5", "participation_rate": "0.25", "latency_ns": 7},
        risk={"max_order_qty": 5, "allow_short": True},
        trade_from=123,
    )
    assert BacktestConfig.from_dict(original.to_dict()) == original


def test_in_process_strategies_are_identified_by_class_source_and_parameters():
    class Tunable(Strategy):
        def __init__(self, lookback, label="x"):
            self.lookback = lookback
            self.label = label
            self._scratch = []  # private state is not a parameter

    class Declared(Strategy):
        def __init__(self, a):
            self.params = {"a": a}
            self.unrelated = 99

    data = dataset(flat_bars("X", 3))
    one = run_backtest(data, Tunable(10), config())
    same = run_backtest(data, Tunable(10), config())
    other = run_backtest(data, Tunable(11), config())
    assert one.strategy["params"] == {"label": "x", "lookback": 10}
    assert one.strategy["name"].endswith("Tunable") and len(one.strategy["source_sha256"]) == 64
    assert one.run == same.run
    assert one.run.config_hash != other.run.config_hash and one.run.id != other.run.id

    declared = run_backtest(data, Declared(1), config())
    assert declared.strategy["params"] == {"a": 1}
    assert declared.run.config_hash != run_backtest(data, Declared(2), config()).run.config_hash
