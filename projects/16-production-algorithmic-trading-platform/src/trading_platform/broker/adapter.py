"""Abstract broker adapter contract separated from OMS and risk business logic."""

from __future__ import annotations

from abc import ABC, abstractmethod
from dataclasses import dataclass, field

from trading_platform.domain.events import (
    BrokerAccountSnapshot,
    BrokerOrderSnapshot,
    FillEvent,
    MarketTick,
    OrderRecord,
    OrderState,
)


@dataclass(frozen=True)
class SubmitResponse:
    broker_order_id: str
    state: OrderState
    fills: list[FillEvent] = field(default_factory=list)
    reject_reason: str | None = None


@dataclass(frozen=True)
class CancelResponse:
    canceled: bool
    reason: str = ""
    racing_fills: list[FillEvent] = field(default_factory=list)


class BrokerAdapter(ABC):
    """Port interface isolating external broker/venue protocols from core trading logic."""

    @abstractmethod
    def is_connected(self) -> bool:
        """Returns True if broker session is healthy and connected."""

    @abstractmethod
    def submit_order(
        self, order: OrderRecord, *, reference_tick: MarketTick | None = None
    ) -> SubmitResponse:
        """Submits an order to the broker venue."""

    @abstractmethod
    def cancel_order(self, order: OrderRecord) -> CancelResponse:
        """Requests cancellation of an open order."""

    @abstractmethod
    def fetch_order(self, client_order_id: str) -> BrokerOrderSnapshot | None:
        """Fetches authoritative broker order state by client_order_id."""

    @abstractmethod
    def fetch_account_snapshot(self) -> BrokerAccountSnapshot:
        """Fetches authoritative broker account cash, positions, and orders for reconciliation."""
