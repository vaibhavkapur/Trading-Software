import dataclasses

import pytest

from backtester.config import ConfigError, ExecutionConfig, RiskLimits
from backtester.fees import FeeModel
from backtester.orders import (
    Liquidity,
    OrderCancel,
    OrderManager,
    OrderStateError,
    OrderStatus,
    OrderType,
    Side,
    SimFill,
    SimOrder,
)
from backtester.risk import (
    INSUFFICIENT_CASH,
    INVALID_LIMIT_PRICE,
    INVALID_QTY,
    MAX_OPEN_ORDERS,
    MAX_ORDER_NOTIONAL,
    MAX_ORDER_QTY,
    MAX_POSITION_QTY,
    NO_MARKET_DATA,
    SHORT_NOT_ALLOWED,
    STALE_MARKET_DATA,
    AccountState,
    RiskGate,
    validate,
)

PX = 1_000_000  # 100.0000


def make_order(oid="O1", side="buy", qty=10, kind="market", limit=None, symbol="X") -> SimOrder:
    return SimOrder(oid, symbol, Side(side), OrderType(kind), qty, limit, 0, decision_px=PX)


def make_fill(order: SimOrder, qty: int, n=1, price=PX, fee=0, ts=5) -> SimFill:
    return SimFill(
        f"F{n}", order.id, order.symbol, order.side, qty, price, fee, ts, Liquidity.TAKER
    )


# -- order lifecycle --------------------------------------------------------------------


def test_order_goes_pending_working_partial_filled():
    oms = OrderManager()
    order = make_order(qty=10)
    assert oms.register(order).status is OrderStatus.PENDING
    oms.on_arrival(order.id, ts=1)
    assert oms.view(order.id).status is OrderStatus.WORKING
    partial = oms.on_fill(make_fill(order, 4, n=1, price=PX, fee=7))
    assert (partial.status, partial.filled_qty, partial.remaining) == (
        OrderStatus.PARTIALLY_FILLED,
        4,
        6,
    )
    assert oms.is_open(order.id)
    done = oms.on_fill(make_fill(order, 6, n=2, price=PX + 10, fee=3, ts=9))
    assert done.status is OrderStatus.FILLED and done.closed_at == 9 and done.fees == 10
    assert done.avg_fill_px == PX + 6  # (4 * PX + 6 * (PX + 10)) / 10
    assert not oms.is_open(order.id) and oms.open_orders() == ()


def test_ids_are_sequential_and_never_reused():
    oms = OrderManager()
    assert [oms.next_id() for _ in range(3)] == ["O0000001", "O0000002", "O0000003"]
    oms.register(make_order("O0000001"))
    with pytest.raises(OrderStateError, match="duplicate"):
        oms.register(make_order("O0000001"))


def test_a_rejected_order_is_closed_at_once_and_never_open():
    oms = OrderManager()
    view = oms.register(make_order(), reject_reason=MAX_ORDER_QTY)
    assert view.status is OrderStatus.REJECTED and view.reason == MAX_ORDER_QTY
    assert not view.is_open and oms.open_count() == 0
    with pytest.raises(OrderStateError):
        oms.on_arrival(view.id, ts=1)


def test_an_order_cannot_be_overfilled():
    oms = OrderManager()
    order = make_order(qty=10)
    oms.register(order)
    oms.on_arrival(order.id, 1)
    oms.on_fill(make_fill(order, 8))
    with pytest.raises(OrderStateError, match="overfill"):
        oms.on_fill(make_fill(order, 3, n=2))
    assert oms.view(order.id).filled_qty == 8


@pytest.mark.parametrize("terminal", ["filled", "canceled"])
def test_nothing_happens_to_an_order_after_it_is_closed(terminal):
    oms = OrderManager()
    order = make_order(qty=10)
    oms.register(order)
    oms.on_arrival(order.id, 1)
    if terminal == "filled":
        oms.on_fill(make_fill(order, 10))
    else:
        oms.on_cancel(OrderCancel(order.id, "USER_CANCEL", 2))
    with pytest.raises(OrderStateError):
        oms.on_fill(make_fill(order, 1, n=2))
    with pytest.raises(OrderStateError):
        oms.on_cancel(OrderCancel(order.id, "USER_CANCEL", 3))


def test_a_fill_cannot_precede_arrival_at_the_exchange():
    oms = OrderManager()
    order = make_order()
    oms.register(order)
    with pytest.raises(OrderStateError):
        oms.on_fill(make_fill(order, 1))


def test_views_are_immutable_snapshots():
    oms = OrderManager()
    order = make_order()
    before = oms.register(order)
    oms.on_arrival(order.id, 1)
    assert before.status is OrderStatus.PENDING  # the old snapshot did not change
    with pytest.raises(dataclasses.FrozenInstanceError):
        before.status = OrderStatus.FILLED


def test_open_orders_filter_by_symbol():
    oms = OrderManager()
    oms.register(make_order("O1", symbol="X"))
    oms.register(make_order("O2", symbol="Y"))
    assert [o.id for o in oms.open_orders("Y")] == ["O2"]
    assert [o.id for o in oms.open_orders()] == ["O1", "O2"]


# -- structural validation --------------------------------------------------------------


@pytest.mark.parametrize("qty", [0, -5, 1.5, True, None])
def test_quantity_must_be_a_positive_integer(qty):
    assert validate(make_order(qty=qty)) == INVALID_QTY


@pytest.mark.parametrize("limit", [None, 0, -1, 1.5])
def test_limit_order_needs_a_positive_integer_price(limit):
    assert validate(make_order(kind="limit", limit=limit)) == INVALID_LIMIT_PRICE


def test_market_order_must_not_carry_a_limit_price():
    assert validate(make_order(kind="market", limit=PX)) == INVALID_LIMIT_PRICE
    assert validate(make_order(kind="market")) is None
    assert validate(make_order(kind="limit", limit=PX)) is None


# -- risk gate --------------------------------------------------------------------------


def account(cash=10_000 * PX, position=0, mark=PX, last=0, now=0, open_orders=()) -> AccountState:
    return AccountState(now, cash, position, mark, last, tuple(open_orders))


def gate(**limits) -> RiskGate:
    return RiskGate(RiskLimits(**limits), FeeModel())


def open_view(order: SimOrder):
    oms = OrderManager()
    return oms.register(order)


def test_order_without_market_data_is_refused():
    assert gate().check(make_order(), account(mark=None, last=None)) == NO_MARKET_DATA


def test_stale_data_boundary():
    g = gate(max_data_age_ns=100)
    assert g.check(make_order(), account(last=0, now=100)) is None  # exactly at the limit
    assert g.check(make_order(), account(last=0, now=101)) == STALE_MARKET_DATA


def test_max_order_qty_boundary():
    g = gate(max_order_qty=10)
    assert g.check(make_order(qty=10), account()) is None
    assert g.check(make_order(qty=11), account()) == MAX_ORDER_QTY


def test_max_order_notional_uses_the_limit_price_or_the_mark():
    g = gate(max_order_notional=10 * PX)
    assert g.check(make_order(qty=10), account()) is None
    assert g.check(make_order(qty=11), account()) == MAX_ORDER_NOTIONAL
    cheap = make_order(qty=20, kind="limit", limit=PX // 2)
    assert g.check(cheap, account()) is None
    assert g.check(make_order(qty=20, kind="limit", limit=PX // 2 + 1), account()) == (
        MAX_ORDER_NOTIONAL
    )


def test_max_position_counts_open_orders_on_the_same_side():
    g = gate(max_position_qty=100)
    resting = open_view(make_order("O9", qty=30))
    assert g.check(make_order(qty=20), account(position=50, open_orders=[resting])) is None
    assert g.check(make_order(qty=21), account(position=50, open_orders=[resting])) == (
        MAX_POSITION_QTY
    )
    other_symbol = open_view(make_order("O8", qty=30, symbol="Y"))
    assert g.check(make_order(qty=50), account(position=50, open_orders=[other_symbol])) is None


def test_max_position_applies_to_shorts_too():
    g = gate(max_position_qty=100, allow_short=True)
    assert g.check(make_order(side="sell", qty=100), account()) is None
    assert g.check(make_order(side="sell", qty=101), account()) == MAX_POSITION_QTY


def test_max_open_orders_boundary():
    g = gate(max_open_orders=1)
    assert g.check(make_order(), account()) is None
    assert g.check(make_order(), account(open_orders=[open_view(make_order("O9"))])) == (
        MAX_OPEN_ORDERS
    )


def test_short_selling_is_refused_unless_allowed():
    assert gate().check(make_order(side="sell", qty=10), account(position=10)) is None
    assert gate().check(make_order(side="sell", qty=11), account(position=10)) == SHORT_NOT_ALLOWED
    assert (
        gate(allow_short=True).check(make_order(side="sell", qty=11), account(position=10)) is None
    )


def test_two_sells_cannot_together_exceed_the_position():
    resting = open_view(make_order("O9", side="sell", qty=6))
    state = account(position=10, open_orders=[resting])
    assert gate().check(make_order(side="sell", qty=4), state) is None
    assert gate().check(make_order(side="sell", qty=5), state) == SHORT_NOT_ALLOWED


def test_cash_check_includes_the_estimated_fee():
    g = RiskGate(RiskLimits(), FeeModel(fee_bps=10))  # 0.1%
    exact = 10 * PX + 10 * PX // 1_000
    assert g.check(make_order(qty=10), account(cash=exact)) is None
    assert g.check(make_order(qty=10), account(cash=exact - 1)) == INSUFFICIENT_CASH
    assert gate(allow_leverage=True).check(make_order(qty=10), account(cash=0)) is None


def test_cash_reserved_by_open_buys_is_not_spent_twice():
    resting = open_view(make_order("O9", qty=6, kind="limit", limit=PX))
    state = account(cash=10 * PX, open_orders=[resting])
    assert gate().check(make_order(qty=4), state) is None
    assert gate().check(make_order(qty=5), state) == INSUFFICIENT_CASH


def test_selling_needs_no_cash():
    assert gate().check(make_order(side="sell", qty=5), account(cash=0, position=5)) is None


# -- configuration ----------------------------------------------------------------------


@pytest.mark.parametrize(
    "kwargs",
    [
        {"fee_bps": "-1"},
        {"slippage_bps": "abc"},
        {"participation_rate": "0"},
        {"participation_rate": "1.5"},
        {"touch_fill_probability": "1.1"},
        {"latency_ns": -1},
        {"latency_ns": 1.5},
    ],
)
def test_execution_config_rejects_nonsense(kwargs):
    with pytest.raises(ConfigError):
        ExecutionConfig(**kwargs)


def test_risk_limits_reject_negative_values():
    with pytest.raises(ConfigError):
        RiskLimits(max_order_qty=-1)
