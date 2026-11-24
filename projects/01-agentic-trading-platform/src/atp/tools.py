"""Tool gateway: the only way a planner can observe the world.

Tools are allowlisted by name and carry a scope. Read tools are available to every
principal; write-scoped tools require an explicit grant, and none are registered in the
MVP. Order placement is not a tool at all: it only happens through the OMS.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass
from typing import Any

from .audit import AuditLog
from .broker.base import ReadOnlyBroker
from .marketdata import SimulatedMarketData

READ = "read"
WRITE = "write"


class ToolNotAllowed(Exception):
    pass


@dataclass(frozen=True)
class Principal:
    user_id: str
    scopes: frozenset[str] = frozenset({READ})


@dataclass(frozen=True)
class Tool:
    name: str
    scope: str
    description: str
    handler: Callable[..., Any]


class ToolGateway:
    def __init__(
        self, market_data: SimulatedMarketData, broker: ReadOnlyBroker, audit: AuditLog
    ) -> None:
        self._md = market_data
        self._broker = broker
        self._audit = audit
        self._tools: dict[str, Tool] = {}
        self.register(Tool("get_quote", READ, "Latest quote for a symbol", self._get_quote))
        self.register(Tool("get_balances", READ, "Cash balance", self._get_balances))
        self.register(Tool("get_positions", READ, "Open positions", self._get_positions))
        self.register(Tool("get_open_orders", READ, "Working orders", self._get_open_orders))

    def register(self, tool: Tool) -> None:
        if tool.scope not in (READ, WRITE):
            raise ValueError(f"unknown scope {tool.scope!r}")
        self._tools[tool.name] = tool

    def describe(self) -> list[dict[str, str]]:
        return [
            {"name": t.name, "scope": t.scope, "description": t.description}
            for t in self._tools.values()
        ]

    def call(self, name: str, principal: Principal, *, correlation_id: str, **args: Any) -> Any:
        tool = self._tools.get(name)
        if tool is None or tool.scope not in principal.scopes:
            self._audit.append(
                "tool.denied",
                correlation_id=correlation_id,
                actor=principal.user_id,
                payload={"tool": name, "args": args},
            )
            raise ToolNotAllowed(f"tool {name!r} is not available to this principal")
        result = tool.handler(principal, **args)
        self._audit.append(
            "tool.called",
            correlation_id=correlation_id,
            actor=principal.user_id,
            payload={"tool": name, "scope": tool.scope, "args": args},
        )
        return result

    # Handlers always act on the authenticated principal, never on a caller-supplied user.
    def _get_quote(self, principal: Principal, *, symbol: str):
        return self._md.get_quote(symbol)

    def _get_balances(self, principal: Principal):
        return self._broker.get_account(principal.user_id).cash

    def _get_positions(self, principal: Principal):
        return self._broker.get_account(principal.user_id).positions

    def _get_open_orders(self, principal: Principal):
        return self._broker.get_open_orders(principal.user_id)
