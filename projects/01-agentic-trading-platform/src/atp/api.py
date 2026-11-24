"""HTTP API (agent-api). Run with: uvicorn atp.api:create_app --factory"""

# No `from __future__ import annotations` here: FastAPI must resolve the locally defined
# `User` dependency alias at decoration time.

import hmac
from decimal import Decimal
from pathlib import Path
from typing import Annotated

from fastapi import Depends, FastAPI, Header, HTTPException, Query, Request, Response
from fastapi.responses import HTMLResponse, JSONResponse
from pydantic import BaseModel, Field

from .config import Settings
from .domain import Decision
from .oms import BrokerUnavailableError
from .service import ServiceError, TradingService

WEB_DIR = Path(__file__).parent / "web"


class IntentRequest(BaseModel):
    text: str = Field(min_length=1, max_length=2000)
    mode: str = "paper"


class ApprovalRequest(BaseModel):
    proposal_hash: str
    decision: Decision


class FaultRequest(BaseModel):
    broker_disconnected: bool | None = None
    broker_order_entry_down: bool | None = None
    broker_drop_next_ack: bool | None = None
    broker_fail_next_submits: int | None = Field(default=None, ge=0, le=100)
    quotes_down: bool | None = None
    quotes_stalled: bool | None = None
    quote_skew_seconds: float | None = None
    price_shock_pct: Decimal | None = None


def create_app(settings: Settings | None = None, service: TradingService | None = None) -> FastAPI:
    settings = settings or (service.settings if service else Settings.from_env())
    svc = service or TradingService(settings)
    app = FastAPI(
        title="Agentic Trading Platform",
        version="0.1.0",
        description="Paper trading only. The planner proposes; deterministic services decide.",
    )
    app.state.service = svc

    def current_user(authorization: Annotated[str | None, Header()] = None) -> str:
        scheme, _, token = (authorization or "").partition(" ")
        if scheme.lower() == "bearer" and token:
            for key, user in settings.api_keys.items():
                if hmac.compare_digest(key.encode(), token.encode()):
                    return user
        raise HTTPException(
            status_code=401,
            detail="missing or invalid API key",
            headers={"WWW-Authenticate": "Bearer"},
        )

    User = Annotated[str, Depends(current_user)]

    @app.exception_handler(ServiceError)
    async def service_error(_: Request, exc: ServiceError) -> JSONResponse:
        return JSONResponse(
            status_code=exc.status,
            content={"error": {"code": exc.code, "message": exc.message, **exc.extra}},
        )

    @app.exception_handler(BrokerUnavailableError)
    async def broker_unavailable(_: Request, exc: BrokerUnavailableError) -> JSONResponse:
        return JSONResponse(
            status_code=503,
            headers={"Retry-After": "2"},
            content={
                "error": {
                    "code": "BROKER_UNAVAILABLE",
                    "message": "broker unreachable; the order was not confirmed. "
                    "Retrying is safe: dispatch is idempotent.",
                    "retryable": True,
                },
                "order": exc.order.model_dump(mode="json"),
            },
        )

    # -- UI and ops ---------------------------------------------------------
    @app.get("/", response_class=HTMLResponse, include_in_schema=False)
    def index() -> str:
        return (WEB_DIR / "index.html").read_text()

    @app.get("/healthz")
    def healthz() -> dict:
        return {"status": "ok", "mode": "paper", "planner": svc.planner.name}

    @app.get("/metrics", include_in_schema=False)
    def metrics() -> Response:
        return Response(svc.metrics.render(), media_type="text/plain; version=0.0.4")

    # -- intents and proposals ---------------------------------------------
    @app.post("/v1/intents", status_code=201)
    def create_intent(
        body: IntentRequest,
        user: User,
        response: Response,
        x_correlation_id: Annotated[str | None, Header(max_length=128)] = None,
    ) -> dict:
        intent, proposal = svc.create_intent(user, body.text, body.mode, x_correlation_id)
        response.headers["X-Correlation-ID"] = intent.correlation_id
        return {"correlation_id": intent.correlation_id, "intent": intent, "proposal": proposal}

    @app.get("/v1/proposals/{proposal_id}")
    def get_proposal(proposal_id: str, user: User) -> dict:
        return svc.proposal_view(user, proposal_id)

    @app.post("/v1/proposals/{proposal_id}/evaluate")
    def evaluate(proposal_id: str, user: User) -> dict:
        decision = svc.evaluate(user, proposal_id)
        return {"risk_decision": decision, "proposal": svc.get_proposal(user, proposal_id)}

    @app.post("/v1/proposals/{proposal_id}/approve")
    def approve(proposal_id: str, body: ApprovalRequest, user: User) -> dict:
        approval, order = svc.decide(user, proposal_id, body.proposal_hash, body.decision)
        return {
            "approval": approval.model_dump(mode="json", exclude={"signature"}),
            "order_id": order.client_order_id if order else None,
        }

    # -- orders -------------------------------------------------------------
    @app.post("/v1/orders/{order_id}/submit")
    def submit(order_id: str, user: User) -> dict:
        order = svc.submit_order(user, order_id)
        return {"order": order, "fills": svc.store.fills(order_id)}

    @app.get("/v1/orders/{order_id}")
    def get_order(order_id: str, user: User) -> dict:
        return svc.get_order(user, order_id)

    @app.post("/v1/orders/{order_id}/cancel")
    def cancel(order_id: str, user: User) -> dict:
        return {"order": svc.cancel_order(user, order_id)}

    @app.get("/v1/orders")
    def list_orders(user: User) -> dict:
        return {"orders": svc.list_orders(user)}

    # -- read-only tools ----------------------------------------------------
    @app.get("/v1/portfolio")
    def portfolio(user: User) -> dict:
        return svc.portfolio(user)

    @app.get("/v1/quotes/{symbol}")
    def quote(symbol: str, user: User) -> dict:
        return {"quote": svc.quote(user, symbol), "simulated": True}

    @app.get("/v1/tools")
    def tools(user: User) -> dict:
        return {"tools": svc.tools.describe()}

    # -- audit --------------------------------------------------------------
    @app.get("/v1/audit")
    def audit(user: User, correlation_id: Annotated[str, Query(min_length=1)]) -> dict:
        return {"correlation_id": correlation_id, "events": svc.audit_trail(user, correlation_id)}

    @app.get("/v1/audit/verify")
    def audit_verify(user: User) -> dict:
        ok, first_bad_seq = svc.audit.verify_chain()
        return {"chain_intact": ok, "first_bad_seq": first_bad_seq}

    # -- simulator fault injection (dev only) -------------------------------
    def _faults() -> dict:
        b, f = svc.broker.faults, svc.market_data.faults
        return {
            "broker_disconnected": b.disconnected,
            "broker_order_entry_down": b.order_entry_down,
            "broker_drop_next_ack": b.drop_next_ack,
            "broker_fail_next_submits": b.fail_next_submits,
            "quotes_down": f.down,
            "quotes_stalled": f.stalled_at is not None,
            "quote_skew_seconds": f.skew_seconds,
            "price_shock_pct": str(f.price_shock_pct),
        }

    if settings.env == "dev":

        @app.get("/v1/sim/faults")
        def get_faults(user: User) -> dict:
            return _faults()

        @app.post("/v1/sim/faults")
        def set_faults(body: FaultRequest, user: User) -> dict:
            b, f = svc.broker.faults, svc.market_data.faults
            if body.broker_disconnected is not None:
                b.disconnected = body.broker_disconnected
            if body.broker_drop_next_ack is not None:
                b.drop_next_ack = body.broker_drop_next_ack
            if body.broker_fail_next_submits is not None:
                b.fail_next_submits = body.broker_fail_next_submits
            if body.broker_order_entry_down is not None:
                b.order_entry_down = body.broker_order_entry_down
            if body.quotes_down is not None:
                f.down = body.quotes_down
            if body.quotes_stalled is not None:
                f.stalled_at = svc.clock.now() if body.quotes_stalled else None
            if body.quote_skew_seconds is not None:
                f.skew_seconds = body.quote_skew_seconds
            if body.price_shock_pct is not None:
                f.price_shock_pct = body.price_shock_pct
            svc.audit.append(
                "sim.faults_changed", correlation_id="sim", actor=user, payload=_faults()
            )
            return _faults()

    return app
