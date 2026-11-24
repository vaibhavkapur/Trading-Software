import random

import pytest

from backtester.orders import Liquidity, Side, SimFill
from backtester.portfolio import LedgerKind, Portfolio, ReconciliationError

CASH = 1_000_000_0000  # 1,000,000.0000


def fill(n, symbol, side, qty, price, fee=0, ts=0) -> SimFill:
    return SimFill(f"F{n}", f"O{n}", symbol, Side(side), qty, price, fee, ts, Liquidity.TAKER)


def book(portfolio: Portfolio, *fills: SimFill) -> list[SimFill]:
    for item in fills:
        portfolio.apply_fill(item)
    return list(fills)


def test_long_round_trip_realizes_the_price_difference():
    p = Portfolio(CASH)
    fills = book(p, fill(1, "X", "buy", 10, 1_000_000), fill(2, "X", "sell", 10, 1_100_000))
    assert p.position_qty("X") == 0
    assert p.realized_pnl() == 10 * 100_000
    assert p.cash == CASH + 1_000_000
    assert p.unrealized_pnl() == 0
    p.reconcile(fills)


def test_short_round_trip_realizes_the_price_difference():
    p = Portfolio(CASH)
    fills = book(p, fill(1, "X", "sell", 10, 1_000_000), fill(2, "X", "buy", 10, 900_000))
    assert p.position_qty("X") == 0
    assert p.realized_pnl() == 10 * 100_000
    p.reconcile(fills)


def test_partial_close_uses_average_cost():
    p = Portfolio(CASH)
    fills = book(
        p,
        fill(1, "X", "buy", 10, 1_000_000),
        fill(2, "X", "buy", 10, 1_200_000),  # average cost 110
        fill(3, "X", "sell", 5, 1_300_000),
    )
    position = p.positions["X"]
    assert position.qty == 15
    assert position.realized_pnl == 5 * 200_000
    assert position.cost_basis == 15 * 1_100_000
    p.mark("X", 1_300_000)
    assert p.unrealized_pnl() == 15 * 200_000
    p.reconcile(fills)


def test_selling_through_zero_closes_the_long_and_opens_a_short_at_the_fill_price():
    p = Portfolio(CASH)
    fills = book(p, fill(1, "X", "buy", 10, 1_000_000), fill(2, "X", "sell", 25, 1_100_000))
    position = p.positions["X"]
    assert position.qty == -15
    assert position.realized_pnl == 10 * 100_000
    assert position.cost_basis == -15 * 1_100_000
    p.mark("X", 1_000_000)
    assert p.unrealized_pnl() == 15 * 100_000
    p.reconcile(fills)


def test_fees_reduce_cash_and_equity_but_not_realized_pnl():
    p = Portfolio(CASH)
    fills = book(
        p,
        fill(1, "X", "buy", 10, 1_000_000, fee=1_234),
        fill(2, "X", "sell", 10, 1_000_000, fee=1_234),
    )
    assert p.realized_pnl() == 0
    assert p.fees == 2_468
    assert p.equity() == CASH - 2_468
    assert [e.kind for e in p.ledger] == [
        LedgerKind.DEPOSIT,
        LedgerKind.TRADE,
        LedgerKind.FEE,
        LedgerKind.TRADE,
        LedgerKind.FEE,
    ]
    p.reconcile(fills)


def test_rounding_in_average_cost_never_leaks_money():
    # 3 units for a total of 100: the per-unit cost is not an integer.
    p = Portfolio(CASH)
    fills = book(p, fill(1, "X", "buy", 1, 33), fill(2, "X", "buy", 2, 34))
    for n in range(3):
        fills += book(p, fill(10 + n, "X", "sell", 1, 40))
    position = p.positions["X"]
    assert position.qty == 0 and position.cost_basis == 0
    assert position.realized_pnl == 3 * 40 - 101
    p.reconcile(fills)


def test_dividend_is_paid_on_longs_charged_on_shorts_and_skipped_when_flat():
    p = Portfolio(CASH)
    fills = book(p, fill(1, "L", "buy", 100, 500_000), fill(2, "S", "sell", 40, 500_000))
    assert p.apply_dividend("L", 5_000, ts=1) == 500_000
    assert p.apply_dividend("S", 5_000, ts=1) == -200_000
    assert p.apply_dividend("NONE", 5_000, ts=1) == 0
    assert p.dividends == 300_000
    p.reconcile(fills)


def test_even_split_changes_quantity_and_mark_but_not_equity():
    p = Portfolio(CASH)
    fills = book(p, fill(1, "X", "buy", 10, 1_000_000))
    p.mark("X", 1_200_000)
    before = p.equity()
    p.apply_split("X", 2, 1, ts=5)
    assert p.position_qty("X") == 20
    assert p.marks["X"] == 600_000
    assert p.positions["X"].cost_basis == 10 * 1_000_000
    assert p.equity() == before
    p.reconcile(fills)


def test_split_pays_cash_for_the_fractional_unit():
    p = Portfolio(CASH)
    fills = book(p, fill(1, "X", "buy", 5, 900_000))
    p.mark("X", 900_000)
    p.apply_split("X", 3, 2, ts=5)  # 7.5 units: keep 7, half a unit at 60 is paid in cash
    assert p.position_qty("X") == 7
    assert p.marks["X"] == 600_000
    entry = p.ledger[-1]
    assert entry.kind is LedgerKind.CASH_IN_LIEU and entry.amount == 300_000
    assert p.equity() == CASH  # bought at the mark, so nothing gained or lost
    p.reconcile(fills)


def test_reverse_split_can_cash_out_a_small_position_entirely():
    p = Portfolio(CASH)
    fills = book(p, fill(1, "X", "buy", 5, 100_000))
    p.mark("X", 120_000)
    p.apply_split("X", 1, 10, ts=5)
    position = p.positions["X"]
    assert position.qty == 0 and position.cost_basis == 0
    assert position.realized_pnl == 5 * 20_000
    p.reconcile(fills)


def test_split_of_a_short_position():
    p = Portfolio(CASH)
    fills = book(p, fill(1, "X", "sell", 5, 900_000))
    p.mark("X", 900_000)
    p.apply_split("X", 3, 2, ts=5)
    assert p.position_qty("X") == -7
    assert p.ledger[-1].amount == -300_000  # the short pays for the fraction
    assert p.equity() == CASH
    p.reconcile(fills)


def test_snapshot_lists_only_open_positions_sorted_by_symbol():
    p = Portfolio(CASH)
    book(
        p,
        fill(1, "B", "buy", 2, 100),
        fill(2, "A", "buy", 3, 200),
        fill(3, "C", "buy", 1, 50),
        fill(4, "C", "sell", 1, 50),
    )
    snap = p.snapshot(ts=9)
    assert snap.holdings == (("A", 3, 200), ("B", 2, 100))
    assert snap.equity == snap.cash + 3 * 200 + 2 * 100 == CASH


@pytest.mark.parametrize("seed", range(25))
def test_accounting_identity_holds_after_every_random_transaction(seed):
    rng = random.Random(seed)
    p = Portfolio(CASH)
    fills: list[SimFill] = []
    symbols = ["A", "B", "C"]
    for n in range(300):
        symbol = rng.choice(symbols)
        roll = rng.random()
        if roll < 0.80:
            item = fill(
                n,
                symbol,
                rng.choice(["buy", "sell"]),
                rng.randrange(1, 500),
                rng.randrange(1, 3_000_000),
                fee=rng.randrange(0, 5_000),
                ts=n,
            )
            p.apply_fill(item)
            fills.append(item)
        elif roll < 0.90:
            p.mark(symbol, rng.randrange(1, 3_000_000))
        elif roll < 0.95:
            p.apply_dividend(symbol, rng.randrange(1, 20_000), ts=n)
        elif symbol in p.marks:
            p.apply_split(symbol, rng.randrange(1, 8), rng.randrange(1, 8), ts=n)
        summary = p.reconcile(fills)
        assert summary["equity"] == p.cash + p.market_value()


def test_reconcile_detects_cash_that_does_not_match_the_ledger():
    p = Portfolio(CASH)
    fills = book(p, fill(1, "X", "buy", 10, 1_000_000))
    p.cash += 1
    with pytest.raises(ReconciliationError, match="cash"):
        p.reconcile(fills)


def test_reconcile_detects_a_position_that_does_not_match_the_fills():
    p = Portfolio(CASH)
    fills = book(p, fill(1, "X", "buy", 10, 1_000_000))
    p.positions["X"].qty += 1
    with pytest.raises(ReconciliationError, match="position"):
        p.reconcile(fills)


def test_reconcile_detects_a_fill_missing_from_the_fill_list():
    p = Portfolio(CASH)
    fills = book(p, fill(1, "X", "buy", 10, 1_000_000), fill(2, "X", "buy", 5, 1_000_000))
    with pytest.raises(ReconciliationError):
        p.reconcile(fills[:1])
