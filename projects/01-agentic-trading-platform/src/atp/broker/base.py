"""Canonical broker interface. Adapters translate this to a specific broker."""

from __future__ import annotations

from decimal import Decimal
from typing import Protocol

from pydantic import BaseModel, Field

from ..domain import AccountSnapshot, Fill, OrderType, Side


class BrokerUnavailable(Exception):
    """Connection-level failure. The outcome of an in-flight request is unknown."""


class BrokerOrderRequest(BaseModel):
    client_order_id: str  # idempotency key
    user_id: str
    symbol: str
    side: Side
    order_type: OrderType
    quantity: int
    limit_price: Decimal | None


class BrokerOrderStatus(BaseModel):
    broker_order_id: str
    client_order_id: str
    user_id: str
    symbol: str
    side: Side
    order_type: OrderType
    quantity: int
    limit_price: Decimal | None
    state: str  # accepted | partially_filled | filled | canceled | rejected
    filled_qty: int
    reject_reason: str | None = None
    fills: list[Fill] = Field(default_factory=list)


class BrokerReadAPI(Protocol):
    def get_account(self, user_id: str) -> AccountSnapshot: ...
    def get_open_orders(self, user_id: str) -> list[BrokerOrderStatus]: ...
    def get_order(self, client_order_id: str) -> BrokerOrderStatus | None: ...


class BrokerAPI(BrokerReadAPI, Protocol):
    def submit_order(self, request: BrokerOrderRequest) -> BrokerOrderStatus: ...
    def cancel_order(self, client_order_id: str) -> BrokerOrderStatus | None: ...


class ReadOnlyBroker:
    """The only broker handle the tool gateway (and therefore the planner) ever gets.

    It structurally has no submit/cancel methods, so a tool cannot place an order
    even if the allowlist were misconfigured.
    """

    def __init__(self, broker: BrokerReadAPI) -> None:
        self._get_account = broker.get_account
        self._get_open_orders = broker.get_open_orders
        self._get_order = broker.get_order

    def get_account(self, user_id: str) -> AccountSnapshot:
        return self._get_account(user_id)

    def get_open_orders(self, user_id: str) -> list[BrokerOrderStatus]:
        return self._get_open_orders(user_id)

    def get_order(self, client_order_id: str) -> BrokerOrderStatus | None:
        return self._get_order(client_order_id)
