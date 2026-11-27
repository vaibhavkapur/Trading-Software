"""Operator Console REST API router and stdlib HTTP server with role-based authorization."""

from __future__ import annotations

import json
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

from trading_platform.domain.errors import AuthorizationError, PlatformError
from trading_platform.domain.events import (
    KillSwitchScope,
    OrderIntent,
    Role,
)
from trading_platform.platform import TradingPlatform

WEB_DIR = Path(__file__).resolve().parent.parent / "web"


class OperatorAPIRouter:
    """Transport-agnostic request router testable directly without sockets."""

    def __init__(self, platform: TradingPlatform) -> None:
        self.platform = platform

    def _parse_role(self, headers: dict[str, str]) -> tuple[str, Role]:
        lowered = {k.lower(): v for k, v in headers.items()}
        actor = lowered.get("x-actor-id", "console-operator")
        raw_role = lowered.get("x-actor-role", "VIEWER").upper()
        try:
            role = Role(raw_role)
        except ValueError as exc:
            raise AuthorizationError(f"Unknown role {raw_role}") from exc
        return actor, role

    def handle(
        self,
        method: str,
        path: str,
        *,
        body: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
    ) -> tuple[int, dict[str, Any]]:
        parsed = urlparse(path)
        route = parsed.path.rstrip("/") or "/"
        hdrs = headers or {}
        payload = body or {}

        try:
            if method == "GET" and route == "/api/health":
                return 200, {
                    "status": "ok",
                    "execution_mode": self.platform.config.execution_policy.execution_mode.value,
                    "broker_connected": self.platform.broker.is_connected(),
                    "ledger_balanced": self.platform.ledger.is_balanced(),
                }

            if method == "GET" and route == "/api/status":
                return 200, self.platform.status_summary()

            if method == "GET" and route == "/api/orders":
                orders = [o.model_dump(mode="json") for o in self.platform.store.list_orders()]
                return 200, {"orders": orders}

            if method == "GET" and route == "/api/fills":
                fills = [f.model_dump(mode="json") for f in self.platform.store.list_fills()]
                return 200, {"fills": fills}

            if method == "GET" and route == "/api/reservations":
                res = [
                    r.model_dump(mode="json")
                    for r in self.platform.store.list_reservations(active_only=False)
                ]
                return 200, {"reservations": res}

            if method == "GET" and route == "/api/ledger":
                tb = {
                    acct: {k: str(v) for k, v in vals.items()}
                    for acct, vals in self.platform.ledger.trial_balance().items()
                }
                positions = [
                    p.model_dump(mode="json")
                    for p in self.platform.store.list_positions(self.platform.config.account_id)
                ]
                return 200, {
                    "balanced": self.platform.ledger.is_balanced(),
                    "cash_balance": str(self.platform.ledger.cash_balance()),
                    "trial_balance": tb,
                    "positions": positions,
                }

            if method == "GET" and route == "/api/reconciliations":
                reports = [
                    r.model_dump(mode="json")
                    for r in self.platform.store.list_reconciliation_reports()
                ]
                return 200, {"reports": reports}

            if method == "GET" and route == "/api/audit":
                events = [
                    e.model_dump(mode="json")
                    for e in self.platform.store.list_audit_events(limit=100)
                ]
                valid, msg = self.platform.store.verify_audit_chain()
                return 200, {"valid": valid, "message": msg, "events": events}

            if method == "GET" and route == "/api/metrics":
                return 200, self.platform.telemetry.snapshot()

            if method == "GET" and route == "/api/live-gate":
                return 200, self.platform.check_live_gate()

            if method == "POST" and route == "/api/orders":
                actor, role = self._parse_role(hdrs)
                if role not in {Role.OPERATOR, Role.CTO}:
                    raise AuthorizationError(
                        f"Role {role.value} not authorized to submit manual paper orders"
                    )
                intent = OrderIntent.model_validate(payload)
                order, risk_res, replayed = self.platform.submit_manual_intent(intent)
                return 200, {
                    "order": order.model_dump(mode="json"),
                    "risk_check": risk_res.model_dump(mode="json") if risk_res else None,
                    "idempotent_replay": replayed,
                }

            if method == "POST" and route.startswith("/api/orders/") and route.endswith("/cancel"):
                actor, role = self._parse_role(hdrs)
                if role not in {Role.OPERATOR, Role.RISK_OFFICER, Role.CTO}:
                    raise AuthorizationError(f"Role {role.value} cannot cancel orders")
                client_order_id = route.split("/")[3]
                order = self.platform.oms.cancel_order(
                    client_order_id,
                    actor=actor,
                    reason=str(payload.get("reason", "Operator cancel")),
                )
                return 200, {"order": order.model_dump(mode="json")}

            if method == "POST" and route == "/api/kill-switch/engage":
                actor, role = self._parse_role(hdrs)
                scope = KillSwitchScope(str(payload.get("scope", "GLOBAL")).upper())
                target = str(payload.get("target", "ALL"))
                reason = str(payload.get("reason", "Operator emergency halt"))
                requires_dual = bool(payload.get("requires_dual_release", False))
                ks, canceled = self.platform.engage_kill_switch(
                    scope=scope,
                    target=target,
                    reason=reason,
                    actor=actor,
                    actor_role=role,
                    requires_dual_release=requires_dual,
                )
                return 200, {
                    "kill_switch": ks.model_dump(mode="json"),
                    "canceled_orders": [o.client_order_id for o in canceled],
                }

            if method == "POST" and route == "/api/kill-switch/release":
                actor, role = self._parse_role(hdrs)
                scope = KillSwitchScope(str(payload.get("scope", "GLOBAL")).upper())
                target = str(payload.get("target", "ALL"))
                reason = str(payload.get("reason", "Operator release"))
                sec_actor = payload.get("secondary_approver")
                sec_role_raw = payload.get("secondary_role")
                sec_role = Role(str(sec_role_raw).upper()) if sec_role_raw else None
                ks = self.platform.release_kill_switch(
                    scope=scope,
                    target=target,
                    reason=reason,
                    actor=actor,
                    actor_role=role,
                    secondary_approver=sec_actor,
                    secondary_role=sec_role,
                )
                return 200, {"kill_switch": ks.model_dump(mode="json")}

            if method == "POST" and route == "/api/reconcile":
                actor, role = self._parse_role(hdrs)
                if role not in {Role.OPERATOR, Role.RISK_OFFICER, Role.COMPLIANCE, Role.CTO}:
                    raise AuthorizationError(f"Role {role.value} cannot trigger reconciliation")
                report = self.platform.reconcile(actor=actor)
                return 200, {"report": report.model_dump(mode="json")}

            return 404, {"error_code": "NOT_FOUND", "message": f"Route {method} {route} not found"}
        except AuthorizationError as exc:
            return 403, exc.to_dict()
        except PlatformError as exc:
            return 400, exc.to_dict()
        except Exception as exc:  # noqa: BLE001
            return 400, {"error_code": "BAD_REQUEST", "message": str(exc)}


def create_http_server(
    platform: TradingPlatform, *, host: str = "127.0.0.1", port: int = 8080
) -> ThreadingHTTPServer:
    router = OperatorAPIRouter(platform)
    index_html = (WEB_DIR / "index.html").read_text(encoding="utf-8")

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, format: str, *args: Any) -> None:  # noqa: A002
            return

        def _send_json(self, status: int, data: dict[str, Any]) -> None:
            raw = json.dumps(data, indent=2, default=str).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def do_GET(self) -> None:  # noqa: N802
            if self.path in {"/", "/index.html"}:
                raw = index_html.encode("utf-8")
                self.send_response(HTTPStatus.OK)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
                return
            status, data = router.handle("GET", self.path, headers=dict(self.headers))
            self._send_json(status, data)

        def do_POST(self) -> None:  # noqa: N802
            length = int(self.headers.get("Content-Length", "0"))
            raw_body = self.rfile.read(length) if length > 0 else b"{}"
            try:
                body = json.loads(raw_body.decode("utf-8"))
            except json.JSONDecodeError:
                self._send_json(400, {"error_code": "INVALID_JSON", "message": "Malformed JSON body"})
                return
            status, data = router.handle(
                "POST", self.path, body=body, headers=dict(self.headers)
            )
            self._send_json(status, data)

    return ThreadingHTTPServer((host, port), Handler)
