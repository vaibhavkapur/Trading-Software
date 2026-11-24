import math

import pytest

from backtester.metrics import compute_metrics, daily_closes, daily_returns, return_stats
from backtester.orders import Liquidity, Side, SimFill
from backtester.portfolio import PortfolioSnapshot
from backtester.timeutil import NS_PER_DAY

HOUR = NS_PER_DAY // 24


def snap(day: float, equity: int, cash: int = 0) -> PortfolioSnapshot:
    return PortfolioSnapshot(int(day * NS_PER_DAY), cash, (), equity)


def fill(order_id, side, qty, price, fee=0, day=0.5) -> SimFill:
    ts = int(day * NS_PER_DAY)
    return SimFill("F", order_id, "X", Side(side), qty, price, fee, ts, Liquidity.TAKER)


def metrics(snapshots, fills=(), decisions=None, base=1_000, **kwargs):
    return compute_metrics(
        snapshots, fills, decisions or {}, base_equity=base, periods_per_year=252, **kwargs
    )


# -- return statistics ------------------------------------------------------------------


def test_return_stats_match_a_hand_calculation():
    returns = [0.01, -0.02, 0.03, 0.00]
    stats = return_stats(returns, periods_per_year=252)
    mean = 0.005
    stdev = math.sqrt(sum((r - mean) ** 2 for r in returns) / 3)
    downside = math.sqrt((0.02**2) / 4)
    assert stats.periods == 4
    assert stats.total_return == pytest.approx(1.01 * 0.98 * 1.03 - 1)
    assert stats.annualized_volatility == pytest.approx(stdev * math.sqrt(252))
    assert stats.sharpe == pytest.approx(mean / stdev * math.sqrt(252))
    assert stats.sortino == pytest.approx(mean / downside * math.sqrt(252))
    assert stats.annualized_return == pytest.approx((1.01 * 0.98 * 1.03) ** (252 / 4) - 1)
    assert stats.max_drawdown == pytest.approx(0.02)


def test_risk_free_rate_is_subtracted_per_period():
    returns = [0.01, -0.02, 0.03, 0.00]
    plain = return_stats(returns, 252)
    with_rf = return_stats(returns, 252, risk_free_rate=0.0252)  # 0.0001 per period
    stdev = plain.annualized_volatility / math.sqrt(252)
    assert plain.sharpe - with_rf.sharpe == pytest.approx(0.0001 / stdev * math.sqrt(252))


def test_undefined_statistics_are_none_not_zero_or_infinity():
    empty = return_stats([], 252)
    assert (empty.periods, empty.total_return, empty.max_drawdown) == (0, 0.0, 0.0)
    assert empty.sharpe is empty.sortino is empty.annualized_volatility is None
    assert empty.annualized_return is None

    single = return_stats([0.05], 252)
    assert single.sharpe is None and single.annualized_volatility is None

    constant = return_stats([0.01, 0.01, 0.01], 252)
    assert constant.sharpe is None  # zero variance
    assert constant.sortino is None  # no downside

    only_up = return_stats([0.01, 0.02, 0.03], 252)
    assert only_up.sharpe is not None and only_up.sortino is None


def test_total_loss_has_no_annualised_return():
    assert return_stats([-1.0, 0.5], 252).annualized_return is None


# -- daily resampling -------------------------------------------------------------------


def test_one_observation_per_utc_day_using_the_last_equity_of_the_day():
    snaps = [snap(0.2, 1_010), snap(0.6, 1_050), snap(1.5, 1_029), snap(3.1, 1_029)]
    assert [e for _, e in daily_closes(snaps)] == [1_050, 1_029, 1_029]
    returns = daily_returns(snaps, base_equity=1_000)
    assert [day for day, _ in returns] == [0, 1, 3]
    assert [r for _, r in returns] == pytest.approx([0.05, -0.02, 0.0])


def test_window_is_half_open():
    snaps = [snap(d, 1_000 + d) for d in range(5)]
    days = [day for day, _ in daily_closes(snaps, start=1 * NS_PER_DAY, end=3 * NS_PER_DAY)]
    assert days == [1, 2]


# -- run metrics ------------------------------------------------------------------------


def test_equity_metrics():
    m = metrics([snap(0.5, 1_100), snap(1.5, 990), snap(2.5, 1_045)])
    assert (m.initial_equity, m.final_equity, m.pnl, m.periods) == (1_000, 1_045, 45, 3)
    assert m.total_return == pytest.approx(0.045)
    assert m.max_drawdown == pytest.approx(0.1)
    assert m.max_drawdown_peak_ts == int(0.5 * NS_PER_DAY)
    assert m.max_drawdown_trough_ts == int(1.5 * NS_PER_DAY)


def test_drawdown_uses_every_snapshot_not_just_daily_closes():
    m = metrics([snap(0.2, 1_000), snap(0.4, 800), snap(0.9, 1_000)])
    assert m.max_drawdown == pytest.approx(0.2)
    assert m.max_drawdown_peak_ts is None  # the peak was the starting equity
    assert m.periods == 1


def test_no_snapshots_means_nothing_happened():
    m = metrics([])
    assert (m.final_equity, m.pnl, m.total_return, m.max_drawdown) == (1_000, 0, 0.0, 0.0)
    assert m.sharpe is None and m.fill_count == 0 and m.shortfall_bps is None


def test_turnover_is_traded_notional_over_mean_daily_equity():
    fills = [fill("O1", "buy", 10, 50), fill("O2", "sell", 10, 60, day=1.5)]
    m = metrics([snap(0.5, 1_000), snap(1.5, 1_200)], fills)
    assert m.traded_notional == 1_100
    assert m.turnover == pytest.approx(1_100 / 1_100)
    assert m.fill_count == 2


def test_shortfall_is_a_cost_when_buying_higher_or_selling_lower_than_decided():
    decisions = {"B": 100, "S": 100}
    worse = metrics([], [fill("B", "buy", 10, 103), fill("S", "sell", 10, 99)], decisions)
    assert worse.shortfall == 10 * 3 + 10 * 1
    assert worse.shortfall_bps == pytest.approx(40 / 2_000 * 10_000)
    better = metrics([], [fill("B", "buy", 10, 98)], decisions)
    assert better.shortfall == -20


def test_fills_without_a_decision_price_are_left_out_of_shortfall():
    m = metrics([], [fill("O1", "buy", 10, 103, fee=4)], {"O1": None})
    assert (m.shortfall, m.shortfall_bps, m.fees, m.fill_count) == (0, None, 4, 1)


def test_fees_and_fills_respect_the_window():
    fills = [fill("O1", "buy", 1, 10, fee=1, day=0.5), fill("O2", "buy", 1, 10, fee=2, day=1.5)]
    m = metrics([snap(0.5, 1_000), snap(1.5, 1_000)], fills, start=NS_PER_DAY)
    assert (m.fees, m.fill_count, m.periods) == (2, 1, 1)


def test_minimum_cash_is_reported():
    m = metrics([snap(0.5, 1_000, cash=400), snap(1.5, 1_000, cash=-25), snap(2.5, 1_000, cash=90)])
    assert m.min_cash == -25
