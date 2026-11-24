"""Immutable events that flow through the simulation.

Every market event carries two clocks. ``ts_event`` is venue time: when the thing happened
on the exchange (a bar closing, a quote changing). ``ts`` is availability time: the earliest
moment a consumer could have known about it. The exchange simulator acts on venue time;
strategies are only ever shown an event once simulated time has reached ``ts``.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import StrEnum


class EventType(StrEnum):
    BAR = "bar"
    QUOTE = "quote"


class ActionType(StrEnum):
    SPLIT = "split"
    DIVIDEND = "dividend"


@dataclass(frozen=True, slots=True)
class Bar:
    """An OHLCV bar covering [ts_open, ts_close]. Prices are integer price units."""

    ts_open: int
    ts_close: int
    open: int
    high: int
    low: int
    close: int
    volume: int


@dataclass(frozen=True, slots=True)
class Quote:
    """Top of book. A price of 0 means that side is empty."""

    bid: int
    ask: int
    bid_size: int
    ask_size: int


@dataclass(frozen=True, slots=True)
class MarketEvent:
    ts: int
    symbol: str
    event_type: EventType
    payload: Bar | Quote
    ts_event: int

    @classmethod
    def bar(cls, symbol: str, bar: Bar, ts_avail: int | None = None) -> MarketEvent:
        """A bar is a venue fact at its close and is published at ``ts_avail``."""
        ts = bar.ts_close if ts_avail is None else ts_avail
        return cls(ts, symbol, EventType.BAR, bar, bar.ts_close)

    @classmethod
    def quote(
        cls, symbol: str, ts_event: int, quote: Quote, ts_avail: int | None = None
    ) -> MarketEvent:
        ts = ts_event if ts_avail is None else ts_avail
        return cls(ts, symbol, EventType.QUOTE, quote, ts_event)


@dataclass(frozen=True, slots=True)
class CorporateAction:
    """A split of ``ratio_num`` new units per ``ratio_den`` old, or a cash dividend per unit."""

    ts: int
    symbol: str
    action_type: ActionType
    ratio_num: int = 1
    ratio_den: int = 1
    amount: int = 0

    @classmethod
    def split(cls, ts: int, symbol: str, num: int, den: int = 1) -> CorporateAction:
        return cls(ts, symbol, ActionType.SPLIT, ratio_num=num, ratio_den=den)

    @classmethod
    def dividend(cls, ts: int, symbol: str, amount: int) -> CorporateAction:
        return cls(ts, symbol, ActionType.DIVIDEND, amount=amount)


@dataclass(frozen=True, slots=True)
class Timer:
    ts: int
    name: str
