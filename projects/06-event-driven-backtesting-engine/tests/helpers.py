"""Small builders shared by the tests. Prices are given as readable decimals."""

from __future__ import annotations

from collections.abc import Callable, Iterable

from backtester import Bar, Context, Dataset, MarketEvent, Quote, Strategy, parse_ts, to_units
from backtester.timeutil import NS_PER_DAY, NS_PER_SEC

DECIMALS = 4
T0 = parse_ts("2024-01-01")  # a Monday
SESSION_OPEN = (14 * 3600 + 30 * 60) * NS_PER_SEC
SESSION = int(6.5 * 3600) * NS_PER_SEC
MINUTE = 60 * NS_PER_SEC
HOUR = 3600 * NS_PER_SEC
DAY = NS_PER_DAY


def px(value: str | int | float) -> int:
    return to_units(str(value), DECIMALS)


def day_open(day: int) -> int:
    return T0 + day * DAY + SESSION_OPEN


def day_close(day: int) -> int:
    return day_open(day) + SESSION


def daily_bar(symbol, day, o, h, low, c, volume=10_000, delay=0) -> MarketEvent:
    """One session-long bar on calendar day ``day`` (0 = Monday 2024-01-01)."""
    bar = Bar(day_open(day), day_close(day), px(o), px(h), px(low), px(c), volume)
    return MarketEvent.bar(symbol, bar, bar.ts_close + delay)


def minute_bar(symbol, index, o, h, low, c, volume=10_000) -> MarketEvent:
    """Contiguous one-minute bars starting at T0."""
    ts_open = T0 + index * MINUTE
    return MarketEvent.bar(
        symbol, Bar(ts_open, ts_open + MINUTE, px(o), px(h), px(low), px(c), volume)
    )


def flat_bars(symbol: str, days: int, price=100, volume=10_000) -> list[MarketEvent]:
    return [daily_bar(symbol, d, price, price, price, price, volume) for d in range(days)]


def quote(symbol, ts, bid, ask, bid_size=1_000, ask_size=1_000, delay=0) -> MarketEvent:
    return MarketEvent.quote(symbol, ts, Quote(px(bid), px(ask), bid_size, ask_size), ts + delay)


def dataset(events: Iterable[MarketEvent], actions=(), **kwargs) -> Dataset:
    return Dataset.build(events, actions, price_decimals=DECIMALS, **kwargs)


class Recorder(Strategy):
    """Records everything it is told, and runs scripted actions.

    ``script`` maps the index of a delivered market event (0 = the first one the strategy
    sees) to a function of the context.
    """

    def __init__(self, script: dict[int, Callable[[Context], None]] | None = None) -> None:
        self.script = script or {}
        self.log: list[tuple] = []
        self.events: list[MarketEvent] = []
        self.fills = []
        self.updates = []
        self.timers = []
        self.actions = []

    def on_market_event(self, event, context):
        index = len(self.events)
        self.events.append(event)
        self.log.append(("data", context.now, event.symbol))
        action = self.script.get(index)
        if action is not None:
            action(context)

    def on_fill(self, fill, context):
        self.fills.append(fill)
        self.log.append(("fill", context.now, fill.order_id))

    def on_order_update(self, order, context):
        self.updates.append(order)
        self.log.append(("update", context.now, order.id, order.status.value, order.reason))

    def on_timer(self, timer, context):
        self.timers.append(timer)
        self.log.append(("timer", context.now, timer.name))

    def on_corporate_action(self, action, context):
        self.actions.append(action)
        self.log.append(("action", context.now, action.symbol))
