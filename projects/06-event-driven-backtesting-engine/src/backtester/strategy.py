"""The strategy contract.

A strategy is a set of callbacks. Everything it can learn about the world arrives as a
callback argument or through the ``Context``, and the context only ever exposes data that
has already been published at the current simulated time. There is no API for asking
about the future, so look-ahead is impossible by construction rather than by convention.

``Context`` is an abstract interface. The backtest engine provides one implementation; a
paper-trading adapter can provide another without the strategy changing.
"""

from __future__ import annotations

import hashlib
import importlib.util
import inspect
import random
import sys
from abc import ABC, abstractmethod
from collections.abc import Mapping
from pathlib import Path
from typing import Any

from .events import Bar, CorporateAction, MarketEvent, Quote, Timer
from .orders import OrderType, OrderView, Side, SimFill, TimeInForce


class Context(ABC):
    """The strategy's only window onto the simulation. Prices are integer price units."""

    @property
    @abstractmethod
    def now(self) -> int:
        """Current simulated time, UTC nanoseconds."""

    @property
    @abstractmethod
    def symbols(self) -> tuple[str, ...]:
        """The tradable universe of this run."""

    @property
    @abstractmethod
    def cash(self) -> int: ...

    @property
    @abstractmethod
    def equity(self) -> int:
        """Cash plus positions valued at the last visible price."""

    @property
    @abstractmethod
    def rng(self) -> random.Random:
        """A seeded random stream. Use this, never the global ``random`` module."""

    @abstractmethod
    def submit_order(
        self,
        symbol: str,
        side: Side | str,
        qty: int,
        order_type: OrderType | str = OrderType.MARKET,
        limit_px: int | None = None,
        tif: TimeInForce | str = TimeInForce.GTC,
        tag: str | None = None,
    ) -> str:
        """Send an order intent through the risk gate. Returns the order id.

        A rejected order is reported through ``Strategy.on_order_update``.
        """

    @abstractmethod
    def cancel_order(self, order_id: str) -> bool:
        """Request a cancel. False if the order is unknown or already closed."""

    @abstractmethod
    def order(self, order_id: str) -> OrderView: ...

    @abstractmethod
    def open_orders(self, symbol: str | None = None) -> tuple[OrderView, ...]: ...

    @abstractmethod
    def position(self, symbol: str) -> int:
        """Signed quantity held."""

    @abstractmethod
    def mark(self, symbol: str) -> int | None:
        """Last visible price (bar close or quote mid), or None before any data."""

    @abstractmethod
    def history(self, symbol: str, n: int | None = None, adjusted: bool = True) -> tuple[Bar, ...]:
        """The last ``n`` published bars, oldest first.

        With adjusted=True, earlier bars are restated for splits that have taken effect
        so far, and only those. With adjusted=False bars are exactly as first published.
        """

    @abstractmethod
    def last_quote(self, symbol: str) -> Quote | None: ...

    @abstractmethod
    def set_timer(self, ts: int, name: str = "") -> None:
        """Ask for ``Strategy.on_timer`` at simulated time ``ts`` (not in the past)."""

    @abstractmethod
    def price(self, value: int | str | float) -> int:
        """Convert a currency amount such as "101.25" to integer price units."""


class Strategy:
    """Base class for strategies. Override the callbacks you need.

    Callbacks must be deterministic functions of what they are given: no wall-clock time,
    no unseeded randomness, no I/O.
    """

    def on_start(self, context: Context) -> None:
        """Called once before the first event."""

    def on_market_event(self, event: MarketEvent, context: Context) -> None:
        """Called when a bar or quote becomes available."""

    def on_fill(self, fill: SimFill, context: Context) -> None:
        """Called after a fill has been booked to the portfolio."""

    def on_order_update(self, order: OrderView, context: Context) -> None:
        """Called when an order is rejected or canceled."""

    def on_timer(self, timer: Timer, context: Context) -> None:
        """Called when a timer set with ``Context.set_timer`` fires."""

    def on_corporate_action(self, action: CorporateAction, context: Context) -> None:
        """Called after a split or dividend has been applied."""

    def on_end(self, context: Context) -> None:
        """Called once after the last event. Orders can no longer be sent."""


class StrategyLoadError(ValueError):
    """The strategy file could not be loaded or does not define a usable strategy."""


def load_strategy_class(path: str | Path, class_name: str | None = None) -> type[Strategy]:
    """Import a strategy file and return its ``Strategy`` subclass."""
    file = Path(path)
    if not file.is_file():
        raise StrategyLoadError(f"strategy file not found: {file}")
    digest = hashlib.sha256(str(file.resolve()).encode()).hexdigest()[:12]
    module_name = f"_backtester_strategy_{digest}"
    spec = importlib.util.spec_from_file_location(module_name, file)
    if spec is None or spec.loader is None:
        raise StrategyLoadError(f"cannot import strategy file: {file}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[module_name] = module
    try:
        spec.loader.exec_module(module)
    except Exception as exc:
        sys.modules.pop(module_name, None)
        raise StrategyLoadError(f"error importing {file}: {exc!r}") from exc

    candidates = {
        name: obj
        for name, obj in vars(module).items()
        if inspect.isclass(obj)
        and issubclass(obj, Strategy)
        and obj is not Strategy
        and obj.__module__ == module_name
    }
    if class_name is not None:
        if class_name not in candidates:
            raise StrategyLoadError(
                f"{file.name} has no Strategy subclass named {class_name!r} "
                f"(found: {', '.join(sorted(candidates)) or 'none'})"
            )
        return candidates[class_name]
    if len(candidates) != 1:
        raise StrategyLoadError(
            f"{file.name} defines {len(candidates)} Strategy subclasses "
            f"({', '.join(sorted(candidates)) or 'none'}); name one explicitly"
        )
    return next(iter(candidates.values()))


def strategy_info(
    path: str | Path, cls: type[Strategy], params: Mapping[str, Any]
) -> dict[str, Any]:
    """Identity of a file-based strategy for the config hash: code and parameters."""
    file = Path(path)
    return {
        "name": cls.__name__,
        "file": file.name,
        "source_sha256": hashlib.sha256(file.read_bytes()).hexdigest(),
        "params": {key: params[key] for key in sorted(params)},
    }
