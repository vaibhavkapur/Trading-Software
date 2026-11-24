"""Input records. Prices and fees are ``Decimal``, quantities are whole shares, timestamps
are UTC microseconds (see ``timeutil``). A single currency is assumed throughout."""

from __future__ import annotations

from dataclasses import dataclass
from decimal import Decimal
from enum import StrEnum


class Side(StrEnum):
    BUY = "buy"
    SELL = "sell"

    @property
    def sign(self) -> int:
        """+1 for buys, -1 for sells. Multiplying (price - benchmark) by this makes a
        positive number mean "worse than the benchmark" for either side."""
        return 1 if self is Side.BUY else -1


@dataclass(frozen=True, slots=True)
class ParentOrder:
    order_id: str
    symbol: str
    side: Side
    qty: int
    decision_ts: int  # when the trading decision was made
    arrival_ts: int  # when the order reached the broker / market
    broker: str
    strategy: str


@dataclass(frozen=True, slots=True)
class ChildOrder:
    child_id: str
    order_id: str
    venue: str
    qty: int
    sent_ts: int


@dataclass(frozen=True, slots=True)
class Fill:
    fill_id: str
    order_id: str
    child_id: str | None
    venue: str
    qty: int
    price: Decimal
    fee: Decimal  # positive = paid, negative = rebate received
    ts: int  # venue execution time


@dataclass(frozen=True, slots=True)
class Quote:
    symbol: str
    venue: str
    bid: Decimal
    ask: Decimal
    ts: int  # venue event time
    recv_ts: int | None  # local receive time, when the capture recorded one


@dataclass(frozen=True, slots=True)
class Bar:
    symbol: str
    ts: int  # bar start, aligned to interval_s
    interval_s: int
    vwap: Decimal
    volume: int

    @property
    def end_ts(self) -> int:
        return self.ts + self.interval_s * 1_000_000


DATASETS = ("orders", "child_orders", "fills", "quotes", "bars")
