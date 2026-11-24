"""Simulated market data.

Prices are a deterministic function of (seed, symbol, time), so a fixed clock and seed
replay identically. This is fixture data, not a real feed.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from decimal import ROUND_HALF_UP, Decimal

from .clock import Clock
from .domain import TICK, Quote


class MarketDataUnavailable(Exception):
    pass


@dataclass
class FeedFaults:
    down: bool = False  # feed raises
    stalled_at: datetime | None = None  # feed keeps serving the last tick from this time
    skew_seconds: float = 0.0  # venue timestamps shifted (clock skew)
    price_shock_pct: Decimal = field(default_factory=lambda: Decimal("0"))


class SimulatedMarketData:
    def __init__(self, clock: Clock, universe: dict[str, str], seed: int = 7) -> None:
        self._clock = clock
        self._base = {s: Decimal(p) for s, p in universe.items()}
        self._seed = seed
        self.faults = FeedFaults()

    @property
    def symbols(self) -> list[str]:
        return sorted(self._base)

    def _mid(self, symbol: str, at: datetime) -> Decimal:
        bucket = int(at.timestamp())
        digest = hashlib.sha256(f"{self._seed}:{symbol}:{bucket}".encode()).digest()
        # Bounded wiggle of +/- 0.25% around the base price.
        unit = Decimal(int.from_bytes(digest[:4], "big")) / Decimal(2**32)
        wiggle = (unit - Decimal("0.5")) * Decimal("0.005")
        shock = Decimal(str(self.faults.price_shock_pct)) / 100
        mid = self._base[symbol] * (1 + wiggle) * (1 + shock)
        return mid.quantize(TICK, rounding=ROUND_HALF_UP)

    def get_quote(self, symbol: str) -> Quote:
        if symbol not in self._base:
            raise KeyError(symbol)
        if self.faults.down:
            raise MarketDataUnavailable("market data feed is down")
        now = self._clock.now()
        event_time = self.faults.stalled_at or now
        mid = self._mid(symbol, event_time)
        return Quote(
            symbol=symbol,
            bid=mid - TICK,
            ask=mid + TICK,
            last=mid,
            event_time=event_time + timedelta(seconds=self.faults.skew_seconds),
            received_at=now,
        )

    # Fault injection -------------------------------------------------------
    def stall(self) -> None:
        self.faults.stalled_at = self._clock.now()

    def resume(self) -> None:
        self.faults = FeedFaults()
