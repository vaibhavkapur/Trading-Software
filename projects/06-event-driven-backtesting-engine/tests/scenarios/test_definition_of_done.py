"""The remaining definition-of-done claims: books reconcile, and reports disclose.

Determinism has its own file under tests/integration; look-ahead has test_no_lookahead.py.
"""

import dataclasses
import re

import pytest
from helpers import DAY
from momentum import Momentum

from backtester import (
    BacktestConfig,
    ExecutionConfig,
    LedgerKind,
    Portfolio,
    ReconciliationError,
    RiskLimits,
    Side,
    Strategy,
    WalkForwardConfig,
    run_backtest,
    walk_forward,
)
from backtester.fixtures import demo_dataset, synthetic_dataset
from backtester.report import render_report, render_walkforward
from backtester.timeutil import NS_PER_MS, fmt_date, fmt_ts

# -- cash and positions reconcile to fills and fees --------------------------------------


class Chaos(Strategy):
    """Trades at random: both sides, both order types, both TIFs, cancels, absurd sizes."""

    def on_market_event(self, event, context):
        rng = context.rng
        for order in context.open_orders():
            if rng.random() < 0.3:
                context.cancel_order(order.id)
        for _ in range(rng.randrange(3)):
            symbol = rng.choice(context.symbols)
            mark = context.mark(symbol)
            if mark is None:
                continue
            side = rng.choice(("buy", "sell"))
            qty = rng.choice((1, 7, 50, 333, 2_000, 40_000))
            tif = rng.choice(("gtc", "ioc"))
            if rng.random() < 0.5:
                context.submit_order(symbol, side, qty, tif=tif)
            else:
                limit = mark * rng.randrange(970, 1031) // 1000
                context.submit_order(symbol, side, qty, "limit", limit, tif=tif)


def chaos_config(seed: int, *, shorts: bool) -> BacktestConfig:
    return BacktestConfig(
        seed=seed,
        execution=ExecutionConfig(
            fee_bps="1.5",
            fee_per_unit=3,
            slippage_bps=4,
            spread_bps=6,
            latency_ns=30 * NS_PER_MS,
            latency_jitter_ns=30 * NS_PER_MS,
            participation_rate="0.001",
            touch_fill_probability="0.4",
        ),
        risk=RiskLimits(allow_short=shorts, allow_leverage=shorts),
    )


@pytest.fixture(scope="module")
def data():
    return demo_dataset(seed=13, days=320)  # split at weekday 300, dividend at 200


@pytest.mark.parametrize("seed", range(8))
@pytest.mark.parametrize("shorts", [False, True])
def test_random_trading_always_reconciles_to_the_unit(data, seed, shorts):
    config = chaos_config(seed, shorts=shorts)
    result = run_backtest(data, Chaos(), config)
    fills, ledger = result.fills, result.ledger
    assert len(fills) > 100

    # Cash is exactly the ledger, and the ledger is exactly the fills plus corporate actions.
    amounts: dict[LedgerKind, int] = {}
    cash = 0
    for entry in ledger:
        cash += entry.amount
        assert entry.cash_after == cash
        amounts[entry.kind] = amounts.get(entry.kind, 0) + entry.amount
    assert amounts[LedgerKind.DEPOSIT] == config.initial_cash
    assert amounts[LedgerKind.TRADE] == -sum(f.side.sign * f.qty * f.px for f in fills)
    assert amounts[LedgerKind.FEE] == -sum(f.fee for f in fills)
    final = result.snapshots[-1]
    assert final.cash == cash

    # Positions are exactly the fills, restated for the split.
    held: dict[str, int] = {}
    for entry in ledger:
        if entry.kind in (LedgerKind.TRADE, LedgerKind.SPLIT):
            held[entry.symbol] = held.get(entry.symbol, 0) + entry.qty
    assert {s: q for s, q in held.items() if q} == {s: q for s, q, _ in final.holdings}

    # The identity the report prints.
    check = result.reconciliation
    assert check["equity"] == final.equity
    assert check["equity"] == (
        config.initial_cash
        + check["realized_pnl"]
        + check["unrealized_pnl"]
        - check["fees"]
        + check["dividends"]
    )
    assert check["fees"] == sum(f.fee for f in fills) == sum(o.fees for o in result.orders)

    # Every order's fills add up, and nothing was filled beyond what was asked.
    by_order: dict[str, int] = {}
    for fill in fills:
        by_order[fill.order_id] = by_order.get(fill.order_id, 0) + fill.qty
    for order in result.orders:
        assert by_order.get(order.id, 0) == order.filled_qty <= max(order.qty, 0)

    if not shorts:
        assert all(s.cash >= 0 for s in result.snapshots)
        assert all(qty >= 0 for s in result.snapshots for _, qty, _ in s.holdings)
        assert result.metrics.min_cash >= 0


def test_the_chaos_runs_exercise_what_they_claim_to(data):
    result = run_backtest(data, Chaos(), chaos_config(1, shorts=True))
    kinds = {entry.kind for entry in result.ledger}
    assert {LedgerKind.SPLIT, LedgerKind.DIVIDEND, LedgerKind.TRADE, LedgerKind.FEE} <= kinds
    assert any(qty < 0 for s in result.snapshots for _, qty, _ in s.holdings)  # shorts
    assert any(0 < o.filled_qty < o.qty for o in result.orders)  # partial fills
    assert result.cancel_reasons.keys() >= {"USER_CANCEL", "IOC_REMAINDER", "CORPORATE_ACTION"}
    assert {f.side for f in result.fills} == {Side.BUY, Side.SELL}
    assert not result.reject_reasons  # nothing to refuse when shorting and leverage are on

    cautious = run_backtest(data, Chaos(), chaos_config(1, shorts=False))
    assert cautious.reject_reasons.keys() == {"INSUFFICIENT_CASH", "SHORT_NOT_ALLOWED"}


def test_a_tampered_fill_is_caught_by_reconciliation(data):
    result = run_backtest(data, Chaos(), chaos_config(2, shorts=True))
    book = Portfolio(result.config.initial_cash, result.snapshots[0].ts)
    fills = list(result.fills)
    for fill in fills:
        book.apply_fill(fill)
    book.reconcile(fills)  # the untouched fills reconcile

    for change in ({"qty": fills[5].qty + 1}, {"px": fills[5].px + 1}, {"fee": fills[5].fee + 1}):
        forged = [*fills[:5], dataclasses.replace(fills[5], **change), *fills[6:]]
        with pytest.raises(ReconciliationError):
            book.reconcile(forged)
    with pytest.raises(ReconciliationError):
        book.reconcile(fills[:-1])


# -- every report discloses assumptions, date ranges, data gaps and the split -----------


def test_the_run_report_discloses_everything_the_blueprint_asks_for(data):
    oos = data.first_ts + 300 * DAY
    config = BacktestConfig(
        seed=5,
        oos_start=oos,
        execution=ExecutionConfig(
            fee_bps=1,
            slippage_bps=3,
            spread_bps=2,
            latency_ns=250 * NS_PER_MS,
            participation_rate="0.05",
        ),
    )
    result = run_backtest(data, Momentum(lookback=20), config)
    report = render_report(result)

    # Simulated, not live; and no claim of profitability.
    assert report.splitlines()[2].startswith("> SIMULATED RESULTS")
    assert "no profitability is implied" in report
    # Date range, both the data's and the evaluated one.
    assert f"from {fmt_ts(data.first_ts)} to {fmt_ts(data.last_ts)}" in report
    assert (
        f"| Period | {fmt_date(result.metrics.start_ts)} to {fmt_date(result.metrics.end_ts)}"
        in report
    )
    # Data gaps: the fixture has four consecutive missing CCC sessions.
    assert "Data gaps (1):" in report and "CCC: 4 missing weekdays" in report
    # Corporate actions that changed the books.
    assert "Corporate actions (2):" in report
    assert "BBB: split 2-for-1" in report and "AAA: dividend 0.5000 per unit" in report
    # Cost and fill assumptions, with the numbers used.
    for line in (
        "Fees: 1 bps of notional",
        "Slippage: 3 bps against the trader",
        "half of an assumed 2 bps bid/ask spread",
        "reach the exchange 250ms after the strategy sends them",
        "capped at 5.00% of each bar's volume",
        "short selling not allowed; buying beyond available cash not allowed",
        "a bar can fill only orders that reached the exchange at or before its open",
        "| Seed | 5 |",
        "Accounting: average cost",
        "annualised with 252 periods per year",
    ):
        assert line in report, line
    # Out-of-sample split, with separate numbers for each side.
    assert "out-of-sample is everything from then on" in report
    assert f"In-sample is everything before {fmt_ts(oos)}" in report
    assert "| Metric | Whole run | In-sample | Out-of-sample |" in report
    assert result.in_sample is not None and result.out_of_sample is not None
    assert result.in_sample.total_return != result.out_of_sample.total_return
    # Identity of the inputs.
    for value in (
        result.run.id,
        result.run.config_hash,
        result.run.data_version,
        result.result_hash,
    ):
        assert value in report


def test_a_report_without_a_split_says_so_plainly(data):
    report = render_report(run_backtest(data, Momentum(lookback=20), BacktestConfig()))
    assert "This run has no out-of-sample split, so every figure below is in-sample" in report
    assert "| Metric | Whole run |\n" in report


def test_undefined_metrics_are_shown_as_undefined_not_as_zero():
    data = synthetic_dataset(("AAA",), days=3, seed=1)
    result = run_backtest(data, Strategy(), BacktestConfig())  # never trades
    assert result.metrics.sharpe is None and result.metrics.sortino is None
    report = render_report(result)
    assert "| Sharpe ratio | n/a |" in report and "| Sortino ratio | n/a |" in report
    assert not re.search(r"\b(nan|inf|infinity)\b", report, re.IGNORECASE)


def test_the_walk_forward_report_discloses_the_same_things(data):
    outcome = walk_forward(
        data,
        lambda params: Momentum(**params),
        BacktestConfig(seed=5, execution=ExecutionConfig(fee_bps=1, slippage_bps=3)),
        WalkForwardConfig(train_ns=120 * DAY, test_ns=60 * DAY, param_grid={"lookback": [10, 30]}),
    )
    report = render_walkforward(outcome)

    assert "> SIMULATED RESULTS" in report
    assert "Data gaps (1):" in report and "CCC: 4 missing weekdays" in report
    assert "Corporate actions (2):" in report
    assert "Fees: 1 bps of notional" in report and "Slippage: 3 bps" in report
    assert "2 parameter combinations were run on the training window" in report
    assert f"{outcome.backtests_run} backtests were run in total" in report
    assert outcome.backtests_run == 3 * len(outcome.folds)
    assert len(outcome.folds) >= 3
    for fold in outcome.folds:
        # Train and test windows are stated, and never overlap.
        assert fold.train_end == fold.test_start < fold.test_end
        assert f"{fmt_date(fold.train_start)} to {fmt_date(fold.train_end)}" in report
        assert f"{fmt_date(fold.test_start)} to {fmt_date(fold.test_end)}" in report
        assert len(fold.candidates) == 2
    assert "| In-sample sharpe | Out-of-sample sharpe |" in report
    assert "A training run is given only data published before its test window begins" in report
    assert "no correction for multiple testing has been applied" in report
