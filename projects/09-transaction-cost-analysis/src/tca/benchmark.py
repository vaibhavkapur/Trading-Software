"""Benchmark prices: consolidated quote mids as of a point in time, and interval VWAP.

Every lookup is point-in-time. A quote is usable for time ``t`` only if its timestamp is at
or before ``t``; when there is none the benchmark is reported as missing. Nothing here ever
reaches forward to the next quote to fill a gap.
"""

from __future__ import annotations

import hashlib
import json
from bisect import bisect_left, bisect_right
from collections import defaultdict
from collections.abc import Iterable
from dataclasses import asdict, dataclass
from decimal import Decimal
from typing import Any

from tca.domain import Bar, Quote
from tca.timeutil import US_PER_MS, US_PER_S

METHODOLOGY_VERSION = "tca-1.0"

ARRIVAL_MID = "arrival_mid"
DECISION_MID = "decision_mid"
INTERVAL_VWAP = "interval_vwap"
BENCHMARKS = (ARRIVAL_MID, DECISION_MID, INTERVAL_VWAP)

OK = "ok"
DEGRADED = "degraded"
MISSING = "missing"

QUOTE_CLOCKS = ("event", "receive")


@dataclass(frozen=True)
class Methodology:
    """Every tunable assumption behind the numbers. ``id`` changes when any of them does,
    so two reports with the same methodology id and dataset version are comparable."""

    version: str = METHODOLOGY_VERSION
    max_quote_age_ms: int = 5_000
    max_clock_skew_ms: int = 250
    quote_clock: str = "event"

    def __post_init__(self) -> None:
        if self.quote_clock not in QUOTE_CLOCKS:
            raise ValueError(f"quote_clock must be one of {QUOTE_CLOCKS}")
        if self.max_quote_age_ms < 0 or self.max_clock_skew_ms < 0:
            raise ValueError("thresholds must be non-negative")

    @property
    def params(self) -> dict[str, Any]:
        return asdict(self)

    @property
    def id(self) -> str:
        digest = hashlib.sha256(json.dumps(self.params, sort_keys=True).encode()).hexdigest()
        return f"{self.version}+{digest[:8]}"


@dataclass(frozen=True)
class Bbo:
    """Best bid and offer across venues as of one instant."""

    bid: Decimal
    ask: Decimal
    bid_venue: str
    ask_venue: str
    quote_ts: int  # clock time of the older of the two quotes setting bid and ask
    age_us: int
    venues: tuple[str, ...]
    stale: bool
    crossed: bool
    clock_skew_us: int | None  # largest |receive - event| on the bid/ask quotes, if recorded
    skewed: bool

    @property
    def mid(self) -> Decimal:
        return (self.bid + self.ask) / 2

    @property
    def status(self) -> str:
        return DEGRADED if (self.stale or self.crossed or self.skewed) else OK


class QuoteBook:
    """Per-venue quote histories with as-of consolidation.

    A venue's quote stands until that venue sends another one or it ages past
    ``max_quote_age_ms``. Venues with a fresh quote are consolidated into a best bid and
    offer. If every venue has aged out, the single most recent quote is returned and marked
    stale so the caller can show the number without trusting it.
    """

    def __init__(self, quotes: Iterable[Quote], methodology: Methodology):
        self._max_age_us = methodology.max_quote_age_ms * US_PER_MS
        self._max_skew_us = methodology.max_clock_skew_ms * US_PER_MS
        use_receive = methodology.quote_clock == "receive"

        grouped: dict[str, dict[str, list[tuple[int, Quote]]]] = defaultdict(
            lambda: defaultdict(list)
        )
        for quote in quotes:
            clock = quote.recv_ts if use_receive and quote.recv_ts is not None else quote.ts
            grouped[quote.symbol][quote.venue].append((clock, quote))

        self._series: dict[str, dict[str, tuple[list[int], list[Quote]]]] = {}
        for symbol, venues in grouped.items():
            self._series[symbol] = {}
            for venue in sorted(venues):
                entries = sorted(venues[venue], key=lambda entry: entry[0])
                self._series[symbol][venue] = (
                    [clock for clock, _ in entries],
                    [quote for _, quote in entries],
                )

    def bbo_at(self, symbol: str, ts: int, *, inclusive: bool = True) -> Bbo | None:
        """Consolidated quote in force at ``ts``.

        ``inclusive=False`` ignores quotes stamped exactly at ``ts``. Fills use that,
        because a quote carrying the same timestamp as a trade may already reflect it.
        """
        latest: list[tuple[int, Quote]] = []
        for times, quotes in self._series.get(symbol, {}).values():
            index = bisect_right(times, ts) if inclusive else bisect_left(times, ts)
            if index:
                latest.append((times[index - 1], quotes[index - 1]))
        if not latest:
            return None

        fresh = [entry for entry in latest if ts - entry[0] <= self._max_age_us]
        pool = fresh or [max(latest, key=lambda entry: entry[0])]
        bid_ts, bid_quote = max(pool, key=lambda entry: (entry[1].bid, entry[0]))
        ask_ts, ask_quote = min(pool, key=lambda entry: (entry[1].ask, -entry[0]))
        quote_ts = min(bid_ts, ask_ts)

        skews = [
            abs(quote.recv_ts - quote.ts)
            for quote in (bid_quote, ask_quote)
            if quote.recv_ts is not None
        ]
        skew = max(skews) if skews else None
        return Bbo(
            bid=bid_quote.bid,
            ask=ask_quote.ask,
            bid_venue=bid_quote.venue,
            ask_venue=ask_quote.venue,
            quote_ts=quote_ts,
            age_us=ts - quote_ts,
            venues=tuple(sorted(quote.venue for _, quote in pool)),
            stale=not fresh,
            crossed=bid_quote.bid > ask_quote.ask,
            clock_skew_us=skew,
            skewed=skew is not None and skew > self._max_skew_us,
        )

    def update_times(self, symbol: str, start: int, end: int) -> list[int]:
        """Clock times in ``[start, end]`` at which any venue updated its quote."""
        times: set[int] = set()
        for venue_times, _ in self._series.get(symbol, {}).values():
            lo = bisect_left(venue_times, start)
            hi = bisect_right(venue_times, end)
            times.update(venue_times[lo:hi])
        return sorted(times)


@dataclass(frozen=True)
class BenchmarkValue:
    name: str
    value: Decimal | None
    status: str  # ok | degraded | missing
    flags: tuple[str, ...]
    details: dict[str, Any]

    @property
    def trusted(self) -> bool:
        return self.status == OK


def quote_benchmark(
    name: str, prefix: str, book: QuoteBook, symbol: str, ts: int
) -> BenchmarkValue:
    """Consolidated mid at or before ``ts``. ``prefix`` namespaces the flags
    (``ARRIVAL_QUOTE_MISSING``, ``DECISION_QUOTE_STALE``, ...)."""
    bbo = book.bbo_at(symbol, ts)
    if bbo is None:
        return BenchmarkValue(name, None, MISSING, (f"{prefix}_QUOTE_MISSING",), {"as_of_ts": ts})
    flags = bbo_flags(bbo, prefix)
    return BenchmarkValue(name, bbo.mid, bbo.status, flags, {"as_of_ts": ts, **bbo_details(bbo)})


def bbo_flags(bbo: Bbo, prefix: str) -> tuple[str, ...]:
    flags = []
    if bbo.stale:
        flags.append(f"{prefix}_QUOTE_STALE")
    if bbo.crossed:
        flags.append(f"{prefix}_QUOTE_CROSSED")
    if bbo.skewed:
        flags.append(f"{prefix}_CLOCK_SKEW")
    return tuple(flags)


def bbo_details(bbo: Bbo) -> dict[str, Any]:
    return {
        "bid": bbo.bid,
        "ask": bbo.ask,
        "bid_venue": bbo.bid_venue,
        "ask_venue": bbo.ask_venue,
        "quote_ts": bbo.quote_ts,
        "quote_age_us": bbo.age_us,
        "venues": list(bbo.venues),
        "clock_skew_us": bbo.clock_skew_us,
    }


class BarBook:
    """Trade bars per symbol. The importer guarantees one interval per symbol and aligned
    starts, so bars never overlap."""

    def __init__(self, bars: Iterable[Bar]):
        self._bars: dict[str, dict[int, Bar]] = defaultdict(dict)
        self._interval_us: dict[str, int] = {}
        for bar in bars:
            self._bars[bar.symbol][bar.ts] = bar
            self._interval_us[bar.symbol] = bar.interval_s * US_PER_S

    def interval_us(self, symbol: str) -> int | None:
        return self._interval_us.get(symbol)

    def slots(self, symbol: str, start: int, end: int) -> list[tuple[int, Bar | None]]:
        """Every bar slot touching ``[start, end]``, with ``None`` where a bar is absent."""
        interval = self._interval_us.get(symbol)
        if interval is None or end < start:
            return []
        first = start - start % interval
        last = end - end % interval
        bars = self._bars[symbol]
        return [(slot, bars.get(slot)) for slot in range(first, last + 1, interval)]


def interval_vwap(bars: BarBook, symbol: str, start: int, end: int) -> BenchmarkValue:
    """Volume-weighted price of all bars touching ``[start, end]``.

    Whole bars are used, so the window is rounded outward to bar boundaries and the result
    includes some trading from just before ``start`` and just after ``end``. When the order
    is shorter than one bar that outside trading dominates, which is flagged as coarse.
    """
    details: dict[str, Any] = {"order_start_ts": start, "order_end_ts": end}
    if end < start:
        return BenchmarkValue(INTERVAL_VWAP, None, MISSING, ("VWAP_WINDOW_INVALID",), details)

    slots = bars.slots(symbol, start, end)
    interval = bars.interval_us(symbol)
    found = [bar for _, bar in slots if bar is not None]
    volume = sum(bar.volume for bar in found)
    if interval is not None:
        details |= {
            "bar_interval_s": interval // US_PER_S,
            "window_start_ts": slots[0][0],
            "window_end_ts": slots[-1][0] + interval,
            "bars_expected": len(slots),
            "bars_found": len(found),
            "volume": volume,
        }
    if volume == 0:
        return BenchmarkValue(INTERVAL_VWAP, None, MISSING, ("VWAP_BARS_MISSING",), details)

    flags = []
    if len(found) < len(slots):
        flags.append("VWAP_BARS_PARTIAL")
    if end - start < interval:
        flags.append("VWAP_BARS_COARSE")
    value = sum((bar.vwap * bar.volume for bar in found), Decimal(0)) / volume
    return BenchmarkValue(INTERVAL_VWAP, value, DEGRADED if flags else OK, tuple(flags), details)
