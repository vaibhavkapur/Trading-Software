"""Fill rules of the exchange simulator, tested directly with integer prices.

Prices are in units of 0.0001, so 1_000_000 is 100.0000 and one basis point of that is 100.
"""

import pytest

from backtester.config import ExecutionConfig
from backtester.events import Bar, MarketEvent, Quote
from backtester.exchange import (
    CORPORATE_ACTION,
    IOC_REMAINDER,
    USER_CANCEL,
    ExchangeSim,
    LatencyModel,
    derive_rng,
)
from backtester.fees import FeeModel
from backtester.orders import (
    Liquidity,
    OrderCancel,
    OrderType,
    Side,
    SimFill,
    SimOrder,
    TimeInForce,
)

BAR = 60  # bar length in the tests' abstract time units


def exchange(**config) -> ExchangeSim:
    cfg = ExecutionConfig(**config)
    return ExchangeSim(cfg, FeeModel(cfg.fee_bps, cfg.fee_per_unit), derive_rng(1, "test"))


def order(oid="O1", side="buy", qty=100, kind="market", limit=None, tif="gtc") -> SimOrder:
    return SimOrder(oid, "X", Side(side), OrderType(kind), qty, limit, 0, TimeInForce(tif))


def bar(ts_open, o, h, low, c, volume=1_000) -> MarketEvent:
    return MarketEvent.bar("X", Bar(ts_open, ts_open + BAR, o, h, low, c, volume))


def quote(ts, bid, ask, bid_size=1_000, ask_size=1_000) -> MarketEvent:
    return MarketEvent.quote("X", ts, Quote(bid, ask, bid_size, ask_size))


def fills(reports) -> list[SimFill]:
    return [r for r in reports if isinstance(r, SimFill)]


def cancels(reports) -> list[OrderCancel]:
    return [r for r in reports if isinstance(r, OrderCancel)]


# -- market orders on bars --------------------------------------------------------------


def test_market_order_fills_at_the_open_of_the_first_bar_that_opens_after_it_arrives():
    ex = exchange()
    assert ex.on_order_arrival(order(), ts=100) == []
    assert ex.on_market_event(bar(60, 990_000, 995_000, 985_000, 991_000)) == []  # opened earlier
    (fill,) = ex.on_market_event(bar(120, 1_000_000, 1_010_000, 990_000, 1_005_000))
    assert (fill.px, fill.qty, fill.ts, fill.liquidity) == (1_000_000, 100, 180, Liquidity.TAKER)
    assert ex.working_count() == 0


def test_an_order_arriving_exactly_at_the_open_is_eligible_for_that_bar():
    ex = exchange()
    ex.on_order_arrival(order(), ts=120)
    assert len(ex.on_market_event(bar(120, 1_000_000, 1_000_000, 1_000_000, 1_000_000))) == 1


def test_buys_pay_and_sells_receive_half_the_spread_plus_slippage():
    ex = exchange(spread_bps=2, slippage_bps=3)  # 1 + 3 = 4 bps = 400 at 100.0000
    ex.on_order_arrival(order("B", "buy"), ts=0)
    ex.on_order_arrival(order("S", "sell"), ts=0)
    buy, sell = ex.on_market_event(bar(0, 1_000_000, 1_000_000, 1_000_000, 1_000_000))
    assert buy.px == 1_000_400
    assert sell.px == 999_600


def test_friction_always_rounds_against_the_trader():
    ex = exchange(slippage_bps=4)  # 4 bps of 1.2345 is 4.938 units
    ex.on_order_arrival(order("B", "buy"), ts=0)
    ex.on_order_arrival(order("S", "sell"), ts=0)
    buy, sell = ex.on_market_event(bar(0, 12_345, 12_345, 12_345, 12_345))
    assert buy.px == 12_350
    assert sell.px == 12_340


def test_a_sell_price_never_goes_to_zero_or_below():
    ex = exchange(slippage_bps=10_000)
    ex.on_order_arrival(order(side="sell"), ts=0)
    (fill,) = ex.on_market_event(bar(0, 5, 5, 5, 5))
    assert fill.px == 1


# -- limit orders on bars ---------------------------------------------------------------


def test_buy_limit_fills_at_its_limit_only_when_the_bar_trades_through():
    ex = exchange()
    ex.on_order_arrival(order(kind="limit", limit=990_000), ts=0)
    assert ex.on_market_event(bar(0, 1_000_000, 1_005_000, 990_001, 1_000_000)) == []
    assert ex.on_market_event(bar(60, 1_000_000, 1_005_000, 990_000, 1_000_000)) == []  # touch
    (fill,) = ex.on_market_event(bar(120, 1_000_000, 1_005_000, 989_999, 1_000_000))
    assert (fill.px, fill.liquidity) == (990_000, Liquidity.MAKER)


def test_sell_limit_fills_at_its_limit_only_when_the_bar_trades_through():
    ex = exchange()
    ex.on_order_arrival(order(side="sell", kind="limit", limit=1_010_000), ts=0)
    assert ex.on_market_event(bar(0, 1_000_000, 1_010_000, 995_000, 1_000_000)) == []  # touch
    (fill,) = ex.on_market_event(bar(60, 1_000_000, 1_010_001, 995_000, 1_000_000))
    assert (fill.px, fill.liquidity) == (1_010_000, Liquidity.MAKER)


def test_passive_limit_fills_pay_no_spread_or_slippage():
    ex = exchange(spread_bps=20, slippage_bps=30)
    ex.on_order_arrival(order(kind="limit", limit=990_000), ts=0)
    (fill,) = ex.on_market_event(bar(0, 1_000_000, 1_000_000, 980_000, 985_000))
    assert fill.px == 990_000


def test_a_touch_fills_with_the_configured_probability():
    never = exchange(touch_fill_probability=0)
    always = exchange(touch_fill_probability=1)
    for ex in (never, always):
        ex.on_order_arrival(order(kind="limit", limit=990_000), ts=0)
    touch = bar(0, 1_000_000, 1_000_000, 990_000, 995_000)
    assert never.on_market_event(touch) == []
    assert fills(always.on_market_event(touch))[0].px == 990_000


def test_touch_fills_are_a_seeded_coin_flip():
    def outcomes(seed: int) -> list[bool]:
        cfg = ExecutionConfig(touch_fill_probability="0.5")
        ex = ExchangeSim(cfg, FeeModel(), derive_rng(seed, "exchange.touch"))
        out = []
        for i in range(200):
            ex.on_order_arrival(order(f"O{i}", kind="limit", limit=990_000, tif="ioc"), ts=i * BAR)
            reports = ex.on_market_event(bar(i * BAR, 1_000_000, 1_000_000, 990_000, 995_000))
            out.append(bool(fills(reports)))
        return out

    first = outcomes(7)
    assert first == outcomes(7)
    assert first != outcomes(8)
    assert 60 < sum(first) < 140


def test_limit_that_is_marketable_at_the_open_fills_at_the_open():
    ex = exchange(slippage_bps=4)
    ex.on_order_arrival(order(kind="limit", limit=990_000), ts=0)
    (fill,) = ex.on_market_event(bar(0, 985_000, 990_000, 980_000, 988_000))  # gapped down
    assert (fill.px, fill.liquidity) == (985_394, Liquidity.TAKER)  # 985_000 + ceil(394.0)


def test_a_taker_limit_fill_is_never_worse_than_the_limit():
    ex = exchange(slippage_bps=4)
    ex.on_order_arrival(order("B", "buy", kind="limit", limit=990_000), ts=0)
    ex.on_order_arrival(order("S", "sell", kind="limit", limit=989_800), ts=0)
    buy, sell = ex.on_market_event(bar(0, 989_900, 995_000, 985_000, 990_000))
    assert buy.px == 990_000  # 989_900 + 396 would breach the limit
    assert sell.px == 989_800  # 989_900 - 396 would breach the limit


# -- partial fills ----------------------------------------------------------------------


def test_participation_cap_spreads_an_order_over_several_bars():
    ex = exchange(participation_rate="0.1")
    ex.on_order_arrival(order(qty=250), ts=0)
    flat = (1_000_000,) * 4
    got = [fills(ex.on_market_event(bar(i * BAR, *flat, volume=1_000))) for i in range(4)]
    assert [[f.qty for f in batch] for batch in got] == [[100], [100], [50], []]


def test_orders_share_a_bars_volume_in_arrival_order():
    ex = exchange(participation_rate="0.1")
    ex.on_order_arrival(order("O1", qty=60), ts=0)
    ex.on_order_arrival(order("O2", qty=60), ts=0)
    got = fills(ex.on_market_event(bar(0, *(1_000_000,) * 4, volume=1_000)))
    assert [(f.order_id, f.qty) for f in got] == [("O1", 60), ("O2", 40)]


def test_a_bar_with_no_volume_fills_nothing_under_a_participation_cap():
    ex = exchange(participation_rate="0.5")
    ex.on_order_arrival(order(), ts=0)
    assert ex.on_market_event(bar(0, *(1_000_000,) * 4, volume=0)) == []
    assert ex.working_count() == 1


def test_ioc_takes_what_it_can_and_cancels_the_rest():
    ex = exchange(participation_rate="0.1")
    ex.on_order_arrival(order(qty=250, tif="ioc"), ts=0)
    fill, cancel = ex.on_market_event(bar(0, *(1_000_000,) * 4, volume=1_000))
    assert fill.qty == 100
    assert (cancel.order_id, cancel.reason) == ("O1", IOC_REMAINDER)
    assert ex.working_count() == 0


def test_ioc_limit_that_cannot_trade_is_canceled_without_a_fill():
    ex = exchange()
    ex.on_order_arrival(order(kind="limit", limit=900_000, tif="ioc"), ts=0)
    (cancel,) = ex.on_market_event(bar(0, *(1_000_000,) * 4))
    assert cancel.reason == IOC_REMAINDER


# -- fees -------------------------------------------------------------------------------


def test_fees_round_up_and_add_the_per_unit_charge():
    ex = exchange(fee_bps=1, fee_per_unit=50)
    ex.on_order_arrival(order(qty=7), ts=0)
    (fill,) = ex.on_market_event(bar(0, 12_350, 12_350, 12_350, 12_350))
    # notional 86_450; 1 bp is 8.645, rounded up to 9; plus 7 * 50
    assert fill.fee == 9 + 350


# -- cancels ----------------------------------------------------------------------------


def test_cancel_removes_a_working_order():
    ex = exchange()
    ex.on_order_arrival(order(), ts=0)
    (cancel,) = ex.on_cancel_request("O1", ts=5)
    assert (cancel.order_id, cancel.reason, cancel.ts) == ("O1", USER_CANCEL, 5)
    assert ex.on_market_event(bar(60, *(1_000_000,) * 4)) == []


def test_cancel_that_overtakes_its_order_cancels_it_on_arrival():
    ex = exchange()
    assert ex.on_cancel_request("O1", ts=1) == []
    (cancel,) = ex.on_order_arrival(order(), ts=2)
    assert (cancel.reason, cancel.ts) == (USER_CANCEL, 2)
    assert ex.working_count() == 0 and ex.late_cancels == 0


def test_cancel_after_the_fill_loses_the_race_and_is_counted():
    ex = exchange()
    ex.on_order_arrival(order(), ts=0)
    ex.on_market_event(bar(0, *(1_000_000,) * 4))
    assert ex.on_cancel_request("O1", ts=100) == []
    assert ex.late_cancels == 1


def test_corporate_action_cancels_every_order_in_the_symbol():
    ex = exchange()
    ex.on_order_arrival(order("O1"), ts=0)
    ex.on_order_arrival(order("O2", kind="limit", limit=1), ts=0)
    reports = ex.cancel_symbol("X", CORPORATE_ACTION, ts=9)
    assert [(c.order_id, c.reason) for c in reports] == [
        ("O1", CORPORATE_ACTION),
        ("O2", CORPORATE_ACTION),
    ]
    ex.cancel_on_arrival("O3", CORPORATE_ACTION)
    assert cancels(ex.on_order_arrival(order("O3"), ts=10))[0].reason == CORPORATE_ACTION


# -- quotes -----------------------------------------------------------------------------


def test_arriving_market_order_trades_against_the_prevailing_quote():
    ex = exchange(slippage_bps=3)
    ex.on_market_event(quote(10, 999_900, 1_000_100))
    (buy,) = ex.on_order_arrival(order("B", "buy"), ts=15)
    (sell,) = ex.on_order_arrival(order("S", "sell"), ts=15)
    assert (buy.px, buy.ts, buy.liquidity) == (1_000_401, 15, Liquidity.TAKER)  # ask + ceil(300.03)
    assert sell.px == 999_600  # bid - ceil(299.97)


def test_a_quote_carries_its_own_spread_so_the_assumed_spread_is_not_charged_twice():
    ex = exchange(spread_bps=50)
    ex.on_market_event(quote(10, 999_900, 1_000_100))
    (fill,) = ex.on_order_arrival(order(), ts=15)
    assert fill.px == 1_000_100


def test_an_order_does_not_trade_against_a_stale_quote():
    ex = exchange(max_quote_age_ns=10)
    ex.on_market_event(quote(0, 999_900, 1_000_100))
    assert ex.on_order_arrival(order(), ts=11) == []
    (fill,) = ex.on_market_event(quote(20, 1_000_900, 1_001_100))
    assert fill.px == 1_001_100


def test_order_with_no_quote_yet_waits_for_one():
    ex = exchange()
    assert ex.on_order_arrival(order(), ts=5) == []
    (fill,) = ex.on_market_event(quote(9, 999_900, 1_000_100))
    assert (fill.px, fill.ts) == (1_000_100, 9)


def test_marketable_limit_takes_at_the_quote_but_never_beyond_its_limit():
    ex = exchange(slippage_bps=3)
    ex.on_market_event(quote(10, 999_900, 1_000_100))
    (roomy,) = ex.on_order_arrival(order("O1", kind="limit", limit=1_001_000), ts=15)
    (tight,) = ex.on_order_arrival(order("O2", kind="limit", limit=1_000_200), ts=15)
    assert roomy.px == 1_000_401
    assert tight.px == 1_000_200


def test_resting_limit_fills_at_its_limit_when_the_book_comes_to_it():
    ex = exchange(slippage_bps=3)
    ex.on_market_event(quote(10, 999_900, 1_000_100))
    assert ex.on_order_arrival(order(kind="limit", limit=1_000_000), ts=15) == []
    assert ex.on_market_event(quote(20, 999_950, 1_000_050)) == []
    (fill,) = ex.on_market_event(quote(30, 999_800, 999_950))
    assert (fill.px, fill.liquidity) == (1_000_000, Liquidity.MAKER)


def test_resting_sell_limit_fills_when_the_bid_reaches_it():
    ex = exchange()
    ex.on_market_event(quote(10, 999_900, 1_000_100))
    assert ex.on_order_arrival(order(side="sell", kind="limit", limit=1_000_000), ts=15) == []
    (fill,) = ex.on_market_event(quote(30, 1_000_000, 1_000_200))
    assert (fill.px, fill.liquidity) == (1_000_000, Liquidity.MAKER)


def test_displayed_size_caps_the_fill_and_is_not_reused_within_a_quote():
    ex = exchange(participation_rate="0.5")
    ex.on_market_event(quote(10, 999_900, 1_000_100, ask_size=300))
    (first,) = ex.on_order_arrival(order("O1", qty=100), ts=11)
    (second,) = ex.on_order_arrival(order("O2", qty=100), ts=12)
    assert (first.qty, second.qty) == (100, 50)  # 150 available in total
    assert ex.on_order_arrival(order("O3", qty=10), ts=13) == []
    refreshed = fills(ex.on_market_event(quote(20, 999_900, 1_000_100, ask_size=300)))
    assert [(f.order_id, f.qty) for f in refreshed] == [("O2", 50), ("O3", 10)]


def test_an_empty_side_of_the_book_fills_nothing():
    ex = exchange()
    ex.on_market_event(quote(10, 999_900, 0, ask_size=0))
    assert ex.on_order_arrival(order("B", "buy"), ts=11) == []
    (sell,) = ex.on_order_arrival(order("S", "sell"), ts=11)
    assert sell.px == 999_900


def test_ioc_against_a_quote_cancels_what_the_size_cannot_cover():
    ex = exchange(participation_rate="1")
    ex.on_market_event(quote(10, 999_900, 1_000_100, ask_size=30))
    fill, cancel = ex.on_order_arrival(order(qty=100, tif="ioc"), ts=11)
    assert fill.qty == 30 and cancel.reason == IOC_REMAINDER


# -- latency and randomness -------------------------------------------------------------


def test_latency_is_fixed_without_jitter_and_bounded_with_it():
    assert {LatencyModel(50, 0, derive_rng(1, "latency")).sample() for _ in range(10)} == {50}
    model = LatencyModel(50, 20, derive_rng(1, "latency"))
    samples = [model.sample() for _ in range(500)]
    assert min(samples) == 50 and max(samples) == 70
    again = LatencyModel(50, 20, derive_rng(1, "latency"))
    assert samples == [again.sample() for _ in range(500)]


def test_random_streams_are_independent_and_reproducible():
    a = [derive_rng(1, "latency").randrange(10**9) for _ in range(3)]
    assert a == [derive_rng(1, "latency").randrange(10**9) for _ in range(3)]
    assert derive_rng(1, "latency").random() != derive_rng(1, "exchange.touch").random()
    assert derive_rng(1, "latency").random() != derive_rng(2, "latency").random()


@pytest.mark.parametrize("side", ["buy", "sell"])
def test_fill_ids_are_sequential(side):
    ex = exchange()
    for i in range(3):
        ex.on_order_arrival(order(f"O{i}", side), ts=0)
    got = ex.on_market_event(bar(0, *(1_000_000,) * 4))
    assert [f.id for f in got] == ["F0000001", "F0000002", "F0000003"]
