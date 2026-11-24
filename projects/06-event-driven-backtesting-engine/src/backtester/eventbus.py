"""Simulated clock and the deterministic event queue.

Events are totally ordered by ``(timestamp, priority, sequence)``. Priority fixes the
causal order of different kinds of event that share a timestamp; sequence (insertion
order) breaks any remaining tie. Nothing in the ordering depends on hashing, object
identity or wall-clock time, so the same inputs always replay in the same order.
"""

from __future__ import annotations

from collections.abc import Iterable
from heapq import heappop, heappush
from typing import Any

# Processing order for events that share a timestamp.
P_CORPORATE_ACTION = 0  # the instrument changes before anything trades
P_VENUE = 1  # the exchange matches resting orders against new market data
P_REQUEST = 2  # order and cancel requests reach the exchange
P_EXECUTION = 3  # fills, cancels and rejects are reported back and booked
P_MARKET_DATA = 4  # market data becomes visible to the strategy
P_TIMER = 5  # strategy timers fire last, seeing everything else at that instant

Entry = tuple[int, int, int, Any]


class ClockError(RuntimeError):
    """Simulated time was asked to move backwards."""


class SimClock:
    """Simulated time. It only moves forward, and only the engine moves it."""

    __slots__ = ("_now",)

    def __init__(self, start: int = 0) -> None:
        self._now = start

    @property
    def now(self) -> int:
        return self._now

    def advance(self, ts: int) -> None:
        if ts < self._now:
            raise ClockError(f"clock cannot move back from {self._now} to {ts}")
        self._now = ts


class EventQueue:
    """Priority queue over a pre-sorted run of historical events plus a heap of new ones.

    Historical data is sorted once and walked with a cursor; only events created during
    the run (order requests, executions, timers) go through the heap.
    """

    __slots__ = ("_heap", "_pos", "_seq", "_static")

    def __init__(self) -> None:
        self._static: list[Entry] = []
        self._pos = 0
        self._heap: list[Entry] = []
        self._seq = 0

    def load(self, items: Iterable[tuple[int, int, Any]]) -> None:
        """Load ``(ts, priority, payload)`` items. Ties keep the order they were given in."""
        if self._seq:
            raise RuntimeError("load() must be called once, before any push()")
        ordered = sorted(items, key=lambda item: (item[0], item[1]))
        self._static = [(ts, prio, seq, payload) for seq, (ts, prio, payload) in enumerate(ordered)]
        self._seq = len(ordered)

    def push(self, ts: int, priority: int, payload: Any) -> None:
        heappush(self._heap, (ts, priority, self._seq, payload))
        self._seq += 1

    def pop(self) -> Entry:
        static, pos, heap = self._static, self._pos, self._heap
        if pos < len(static):
            # Sequence numbers are unique, so tuple comparison never reaches the payload.
            if heap and heap[0] < static[pos]:
                return heappop(heap)
            self._pos = pos + 1
            return static[pos]
        if heap:
            return heappop(heap)
        raise IndexError("pop from an empty event queue")

    def __len__(self) -> int:
        return len(self._static) - self._pos + len(self._heap)

    def __bool__(self) -> bool:
        return self._pos < len(self._static) or bool(self._heap)
