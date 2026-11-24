"""Exchange simulator: latency, matching, partial fills, spread and slippage.

The simulator sees market data at venue time and decides what an order would have
traded. Its rules are deliberately pessimistic wherever the data cannot tell us what
really happened.

Bars
  * A bar can only fill an order that reached the exchange at or before the bar opened.
    An order that arrives mid-bar waits for the next one, because the open, high and low
    it would be matched against may all have happened before it arrived.
  * Market orders fill at the bar's open, moved against the trader by half the assumed
    spread plus slippage.
  * A limit order that is marketable at the open fills at the open (same adjustment,
    never worse than its limit). Otherwise it fills at its limit price only if the bar
    trades through that price. A bar that merely touches the limit fills it with
    probability ``touch_fill_probability`` (default 0), since queue position is unknown.
  * Fills are booked at the bar's close, when the bar is first a complete venue fact.
  * Total quantity filled from one bar is capped at ``participation_rate`` of its volume;
    the rest of the order keeps working.

Quotes
  * An arriving order trades against the prevailing quote, if it is fresh enough.
  * Takers pay the quoted price plus slippage. A resting limit order fills at its limit
    when the opposite side of the book reaches it.
  * Quantity is capped at ``participation_rate`` of the displayed size.

See docs/decisions/0001-bar-fill-eligibility.md for why the bar rules are what they are.
"""

from __future__ import annotations

import hashlib
import random
from decimal import Decimal
from fractions import Fraction

from .config import ExecutionConfig
from .events import Bar, EventType, MarketEvent, Quote
from .fees import FeeModel
from .fixedpoint import Ratio
from .orders import (
    Liquidity,
    OrderCancel,
    OrderType,
    Side,
    SimFill,
    SimOrder,
    TimeInForce,
)

ExecutionReport = SimFill | OrderCancel

USER_CANCEL = "USER_CANCEL"
IOC_REMAINDER = "IOC_REMAINDER"
CORPORATE_ACTION = "CORPORATE_ACTION"

_PROBABILITY_SCALE = 1_000_000


def derive_rng(seed: int, stream: str) -> random.Random:
    """An independent, reproducible random stream for one purpose.

    Separate streams mean that enabling one stochastic feature does not shift the draws
    seen by another.
    """
    digest = hashlib.sha256(f"{seed}:{stream}".encode()).digest()
    return random.Random(int.from_bytes(digest[:8], "big"))


class LatencyModel:
    """Fixed delay plus uniform integer jitter from a seeded stream."""

    __slots__ = ("_base", "_jitter", "_rng")

    def __init__(self, base_ns: int, jitter_ns: int, rng: random.Random) -> None:
        self._base = base_ns
        self._jitter = jitter_ns
        self._rng = rng

    def sample(self) -> int:
        if self._jitter:
            return self._base + self._rng.randrange(self._jitter + 1)
        return self._base


class _Working:
    __slots__ = ("arrived_at", "order", "remaining", "rested")

    def __init__(self, order: SimOrder, arrived_at: int) -> None:
        self.order = order
        self.remaining = order.qty
        self.arrived_at = arrived_at
        self.rested = False  # True once the order has sat through a quote without crossing


class _BookTop:
    __slots__ = ("ask_left", "bid_left", "quote", "ts")

    def __init__(self, quote: Quote, ts: int, bid_left: int | None, ask_left: int | None) -> None:
        self.quote = quote
        self.ts = ts
        self.bid_left = bid_left
        self.ask_left = ask_left


class ExchangeSim:
    def __init__(self, config: ExecutionConfig, fees: FeeModel, rng: random.Random) -> None:
        self._fees = fees
        self._rng = rng
        self._slippage = Ratio.from_bps(config.slippage_bps)
        self._bar_cost = Ratio.from_bps(config.spread_bps / 2 + config.slippage_bps)
        self._participation = (
            None if config.participation_rate is None else Ratio.parse(config.participation_rate)
        )
        self._touch = int(Fraction(Decimal(config.touch_fill_probability)) * _PROBABILITY_SCALE)
        self._max_quote_age = config.max_quote_age_ns
        self._books: dict[str, list[_Working]] = {}
        self._by_id: dict[str, _Working] = {}
        self._arrived: set[str] = set()
        self._cancel_on_arrival: dict[str, str] = {}
        self._tops: dict[str, _BookTop] = {}
        self._fill_seq = 0
        self.late_cancels = 0

    # -- requests from the trader ---------------------------------------------------

    def on_order_arrival(self, order: SimOrder, ts: int) -> list[ExecutionReport]:
        self._arrived.add(order.id)
        reason = self._cancel_on_arrival.pop(order.id, None)
        if reason is not None:
            return [OrderCancel(order.id, reason, ts)]
        working = _Working(order, ts)
        self._books.setdefault(order.symbol, []).append(working)
        self._by_id[order.id] = working

        out: list[ExecutionReport] = []
        top = self._tops.get(order.symbol)
        if top is not None and (self._max_quote_age is None or ts - top.ts <= self._max_quote_age):
            self._match_quote(working, top, ts, out)
        return out

    def on_cancel_request(self, order_id: str, ts: int) -> list[ExecutionReport]:
        working = self._by_id.get(order_id)
        if working is not None:
            return [self._cancel(working, USER_CANCEL, ts)]
        if order_id in self._arrived:
            self.late_cancels += 1  # already filled or canceled: the cancel lost the race
        else:
            self._cancel_on_arrival.setdefault(order_id, USER_CANCEL)
        return []

    def cancel_on_arrival(self, order_id: str, reason: str) -> None:
        """Cancel an order that is still in flight as soon as it arrives."""
        self._cancel_on_arrival.setdefault(order_id, reason)

    def cancel_symbol(self, symbol: str, reason: str, ts: int) -> list[ExecutionReport]:
        self._tops.pop(symbol, None)
        return [self._cancel(w, reason, ts) for w in tuple(self._books.get(symbol, ()))]

    # -- market data at venue time --------------------------------------------------

    def on_market_event(self, event: MarketEvent) -> list[ExecutionReport]:
        out: list[ExecutionReport] = []
        symbol = event.symbol
        if event.event_type is EventType.BAR:
            book = self._books.get(symbol)
            if book:
                self._match_bar(book, event.payload, event.ts_event, out)
        else:
            quote = event.payload
            top = _BookTop(
                quote,
                event.ts_event,
                self._budget(quote.bid_size),
                self._budget(quote.ask_size),
            )
            self._tops[symbol] = top
            for working in tuple(self._books.get(symbol, ())):
                self._match_quote(working, top, event.ts_event, out)
        return out

    def working_count(self) -> int:
        return len(self._by_id)

    # -- matching -------------------------------------------------------------------

    def _match_bar(
        self, book: list[_Working], bar: Bar, ts: int, out: list[ExecutionReport]
    ) -> None:
        budget = self._budget(bar.volume)
        for working in tuple(book):
            if working.arrived_at > bar.ts_open:
                continue
            priced = self._bar_price(working.order, bar)
            if priced is not None:
                qty = working.remaining if budget is None else min(working.remaining, budget)
                if qty > 0:
                    if budget is not None:
                        budget -= qty
                    out.append(self._fill(working, qty, priced[0], priced[1], ts))
            if working.remaining and working.order.tif is TimeInForce.IOC:
                out.append(self._cancel(working, IOC_REMAINDER, ts))

    def _bar_price(self, order: SimOrder, bar: Bar) -> tuple[int, Liquidity] | None:
        buying = order.side is Side.BUY
        taker_px = self._adverse(bar.open, self._bar_cost, buying)
        if order.type is OrderType.MARKET:
            return taker_px, Liquidity.TAKER
        limit = order.limit_px
        if buying:
            if bar.open <= limit:
                return min(limit, taker_px), Liquidity.TAKER
            if bar.low < limit or (bar.low == limit and self._touched()):
                return limit, Liquidity.MAKER
        else:
            if bar.open >= limit:
                return max(limit, taker_px), Liquidity.TAKER
            if bar.high > limit or (bar.high == limit and self._touched()):
                return limit, Liquidity.MAKER
        return None

    def _match_quote(
        self, working: _Working, top: _BookTop, ts: int, out: list[ExecutionReport]
    ) -> None:
        order = working.order
        buying = order.side is Side.BUY
        ref = top.quote.ask if buying else top.quote.bid
        left = top.ask_left if buying else top.bid_left
        was_resting = working.rested
        working.rested = True

        px: int | None = None
        liquidity = Liquidity.TAKER
        if ref > 0:
            taker_px = self._adverse(ref, self._slippage, buying)
            if order.type is OrderType.MARKET:
                px = taker_px
            else:
                limit = order.limit_px
                crossed = ref <= limit if buying else ref >= limit
                if crossed and was_resting:
                    px, liquidity = limit, Liquidity.MAKER
                elif crossed:
                    px = min(limit, taker_px) if buying else max(limit, taker_px)
        if px is not None:
            qty = working.remaining if left is None else min(working.remaining, left)
            if qty > 0:
                if left is not None:
                    if buying:
                        top.ask_left = left - qty
                    else:
                        top.bid_left = left - qty
                out.append(self._fill(working, qty, px, liquidity, ts))
        if working.remaining and order.tif is TimeInForce.IOC:
            out.append(self._cancel(working, IOC_REMAINDER, ts))

    # -- helpers --------------------------------------------------------------------

    @staticmethod
    def _adverse(px: int, cost: Ratio, buying: bool) -> int:
        """Move a price against the trader, rounding the cost up to a whole price unit."""
        move = cost.ceil(px)
        return px + move if buying else max(1, px - move)

    def _budget(self, displayed: int) -> int | None:
        if self._participation is None:
            return None
        return self._participation.floor(displayed)

    def _touched(self) -> bool:
        if self._touch <= 0:
            return False
        return self._rng.randrange(_PROBABILITY_SCALE) < self._touch

    def _fill(self, working: _Working, qty: int, px: int, liquidity: Liquidity, ts: int) -> SimFill:
        order = working.order
        working.remaining -= qty
        if working.remaining == 0:
            self._remove(working)
        self._fill_seq += 1
        return SimFill(
            id=f"F{self._fill_seq:07d}",
            order_id=order.id,
            symbol=order.symbol,
            side=order.side,
            qty=qty,
            px=px,
            fee=self._fees.fee(qty * px, qty),
            ts=ts,
            liquidity=liquidity,
        )

    def _cancel(self, working: _Working, reason: str, ts: int) -> OrderCancel:
        self._remove(working)
        return OrderCancel(working.order.id, reason, ts)

    def _remove(self, working: _Working) -> None:
        del self._by_id[working.order.id]
        self._books[working.order.symbol].remove(working)
