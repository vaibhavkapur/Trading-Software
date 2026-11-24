"""FastAPI application exposing options analytics, risk scenarios, and paper orders."""

from __future__ import annotations

import os
import time
from datetime import UTC, date, datetime
from pathlib import Path

from fastapi import FastAPI, HTTPException, Query, Response
from fastapi.responses import HTMLResponse

from options_infra import __version__
from options_infra.api.schemas import (
    ChainItem,
    ChainResponse,
    IVRequest,
    IVResponse,
    PaperOrderRequest,
    PaperOrderResponse,
    PortfolioGreeksRequest,
    PortfolioGreeksResponse,
    PriceRequest,
    PriceResponse,
    VerticalSpreadPreviewRequest,
)
from options_infra.domain.models import (
    ModelWarning,
    MultiLegOrder,
    OptionPosition,
    OptionQuote,
    PricingModelType,
    WarningSeverity,
)
from options_infra.instruments.repository import (
    ClockSkewError,
    InstrumentNotFoundError,
    InstrumentRepository,
    QuoteFeedDisconnectedError,
    QuoteNotFoundError,
    StaleQuoteError,
)
from options_infra.metrics.registry import OptionsMetricsRegistry
from options_infra.paper_adapter.adapter import PaperMultiLegAdapter
from options_infra.paper_adapter.broker_rules import BrokerCapabilities
from options_infra.paper_adapter.pre_trade_risk import PreTradeRiskConfig
from options_infra.paper_adapter.store import PaperOrderStore, StoreWriteError
from options_infra.pricing.binomial import price_binomial_crr
from options_infra.pricing.black_scholes import (
    BSMInput,
    price_bsm,
    put_call_parity_residual,
)
from options_infra.pricing.iv_solver import solve_implied_volatility
from options_infra.risk.aggregator import aggregate_portfolio_greeks, compute_contract_greeks
from options_infra.risk.scenarios import run_portfolio_scenarios
from options_infra.strategy.vertical_spread import (
    SpreadValidationError,
    VerticalSpreadPreview,
    build_vertical_spread_preview,
)

WEB_INDEX_PATH = Path(__file__).resolve().parent.parent / "web" / "index.html"


def create_app(
    repository: InstrumentRepository | None = None,
    store: PaperOrderStore | None = None,
    capabilities: BrokerCapabilities | None = None,
    risk_config: PreTradeRiskConfig | None = None,
    metrics: OptionsMetricsRegistry | None = None,
) -> FastAPI:
    """Create a configured FastAPI application instance."""
    if repository is None:
        fixture_env = os.environ.get("OPTIONS_INFRA_FIXTURE_PATH")
        max_age = float(os.environ.get("OPTIONS_INFRA_MAX_QUOTE_AGE_SECONDS", "300"))
        max_skew = float(os.environ.get("OPTIONS_INFRA_MAX_CLOCK_SKEW_SECONDS", "30"))
        if fixture_env and Path(fixture_env).exists():
            repository = InstrumentRepository.from_fixture(
                Path(fixture_env),
                max_quote_age_seconds=max_age,
                max_clock_skew_seconds=max_skew,
            )
        else:
            repository = InstrumentRepository.from_default_simulator(
                max_quote_age_seconds=max_age,
                max_clock_skew_seconds=max_skew,
            )

    if store is None:
        db_path = os.environ.get("OPTIONS_INFRA_DB_PATH", ":memory:")
        store = PaperOrderStore(db_path=db_path)

    metrics_reg = metrics or OptionsMetricsRegistry()
    adapter = PaperMultiLegAdapter(
        repository=repository,
        store=store,
        capabilities=capabilities,
        risk_config=risk_config,
    )

    api = FastAPI(
        title="Options Trading Infrastructure (PAPER / SIMULATED)",
        version=__version__,
        description=(
            "Defensible European Black-Scholes-Merton analytics, IV solver, portfolio Greeks "
            "& shock scenarios, and defined-risk vertical spread paper order preview."
        ),
    )

    # Expose components on app.state for integration tests and operational inspection
    api.state.repository = repository
    api.state.store = store
    api.state.adapter = adapter
    api.state.metrics = metrics_reg

    @api.get("/", response_class=HTMLResponse, include_in_schema=False)
    def ui_home() -> HTMLResponse:
        if not WEB_INDEX_PATH.exists():
            return HTMLResponse("<h1>Options Trading Infrastructure (PAPER)</h1>")
        return HTMLResponse(WEB_INDEX_PATH.read_text(encoding="utf-8"))

    @api.get("/health")
    def health() -> dict[str, object]:
        return {
            "status": "ok" if repository.feed_connected else "degraded",
            "version": __version__,
            "execution_mode": "PAPER_SIMULATED",
            "feed_connected": repository.feed_connected,
            "symbols": repository.list_symbols(),
            "latest_quote_asof": (
                repository.latest_quote_time().isoformat()
                if repository.latest_quote_time()
                else None
            ),
        }

    @api.get("/metrics", include_in_schema=False)
    def prometheus_metrics() -> Response:
        return Response(
            content=metrics_reg.render_prometheus(),
            media_type="text/plain; version=0.0.4; charset=utf-8",
        )

    @api.get("/v1/metrics")
    def json_metrics() -> dict[str, object]:
        return metrics_reg.snapshot_json()

    @api.get("/v1/options/symbols")
    def list_symbols() -> dict[str, list[str]]:
        return {
            sym: [exp.isoformat() for exp in repository.list_expiries(sym)]
            for sym in repository.list_symbols()
        }

    @api.get("/v1/options/chain", response_model=ChainResponse)
    def get_option_chain(
        symbol: str = Query(..., description="Underlying ticker symbol, e.g. XYZ"),
        expiry: date = Query(..., description="Option expiration date YYYY-MM-DD"),
        valuation_time: datetime | None = Query(
            default=None, description="Optional UTC timestamp for IV/Greek valuation"
        ),
    ) -> ChainResponse:
        try:
            pairs = repository.get_chain(symbol=symbol, expiry=expiry)
        except QuoteFeedDisconnectedError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc

        if not pairs:
            raise HTTPException(
                status_code=404,
                detail=(
                    f"No option chain found for symbol={symbol.upper()} "
                    f"expiry={expiry.isoformat()}"
                ),
            )

        rows: list[ChainItem] = []
        chain_warnings: list[ModelWarning] = []
        seen_chain_codes: set[str] = set()
        underlying_px = None
        chain_asof = None
        chain_received_at = None

        for contract, quote in pairs:
            row_warnings: list[ModelWarning] = []
            greeks = None
            if quote is not None:
                underlying_px = quote.underlying_px
                chain_asof = quote.asof
                chain_received_at = quote.received_at
                val_ref = valuation_time or quote.asof
                row_warnings.extend(repository.inspect_quote_health(quote, reference_now=val_ref))
                snap, _, calc_warns = compute_contract_greeks(
                    contract=contract,
                    quote=quote,
                    valuation_time=val_ref,
                )
                metrics_reg.record_iv_result(converged=snap.iv is not None)
                greeks = snap
                row_warnings.extend(calc_warns)
            else:
                row_warnings.append(
                    ModelWarning(
                        code="MISSING_QUOTE",
                        severity=WarningSeverity.WARNING,
                        message=f"No quote available for {contract.id}.",
                    )
                )

            for w in row_warnings:
                if w.code not in seen_chain_codes:
                    seen_chain_codes.add(w.code)
                    chain_warnings.append(w)

            rows.append(
                ChainItem(
                    contract=contract,
                    quote=quote,
                    greeks=greeks,
                    warnings=row_warnings,
                )
            )

        return ChainResponse(
            symbol=symbol.upper(),
            expiry=expiry.isoformat(),
            underlying_px=underlying_px,
            asof=chain_asof,
            received_at=chain_received_at,
            rows=rows,
            warnings=chain_warnings,
        )

    @api.post("/v1/options/price", response_model=PriceResponse)
    def price_option(req: PriceRequest) -> PriceResponse:
        try:
            if req.model == PricingModelType.BINOMIAL_CRR:
                res = price_binomial_crr(
                    right=req.right,
                    spot=req.spot,
                    strike=req.strike,
                    years=req.years,
                    rate=req.rate,
                    dividend_yield=req.dividend_yield,
                    volatility=req.volatility,
                    style=req.style,
                    steps=req.binomial_steps,
                )
                intrinsic = (
                    max(req.spot - req.strike, 0.0)
                    if req.right.value == "CALL"
                    else max(req.strike - req.spot, 0.0)
                )
                return PriceResponse(
                    price=res.price,
                    delta=res.delta,
                    gamma=res.gamma,
                    theta=res.theta,
                    theta_per_day=res.theta / 365.0,
                    intrinsic_value=intrinsic,
                    time_value=res.price - intrinsic,
                    early_exercise_premium=res.early_exercise_premium,
                    assumptions=res.assumptions,
                    warnings=list(res.warnings),
                )

            bsm_res = price_bsm(
                BSMInput(
                    right=req.right,
                    spot=req.spot,
                    strike=req.strike,
                    years=req.years,
                    rate=req.rate,
                    dividend_yield=req.dividend_yield,
                    volatility=req.volatility,
                    style=req.style,
                )
            )
            parity_err = put_call_parity_residual(
                spot=req.spot,
                strike=req.strike,
                years=req.years,
                rate=req.rate,
                dividend_yield=req.dividend_yield,
                volatility=req.volatility,
            )
            metrics_reg.record_pricing_error(abs(parity_err))
            return PriceResponse(
                price=bsm_res.price,
                delta=bsm_res.delta,
                gamma=bsm_res.gamma,
                vega=bsm_res.vega,
                vega_per_1pct=bsm_res.vega_per_1pct,
                theta=bsm_res.theta,
                theta_per_day=bsm_res.theta_per_day,
                rho=bsm_res.rho,
                intrinsic_value=bsm_res.intrinsic_value,
                time_value=bsm_res.time_value,
                put_call_parity_residual=parity_err,
                assumptions=bsm_res.assumptions,
                warnings=list(bsm_res.warnings),
            )
        except ValueError as exc:
            raise HTTPException(status_code=422, detail=str(exc)) from exc

    @api.post("/v1/options/iv", response_model=IVResponse)
    def invert_iv(req: IVRequest) -> IVResponse:
        res = solve_implied_volatility(
            market_price=req.market_price,
            right=req.right,
            spot=req.spot,
            strike=req.strike,
            years=req.years,
            rate=req.rate,
            dividend_yield=req.dividend_yield,
        )
        metrics_reg.record_iv_result(converged=res.converged)
        if res.pricing_error is not None:
            metrics_reg.record_pricing_error(abs(res.pricing_error))
        return IVResponse(
            converged=res.converged,
            implied_volatility=res.implied_volatility,
            iterations=res.iterations,
            pricing_error=res.pricing_error,
            status=res.status,
            message=res.message,
        )

    @api.post("/v1/portfolio/greeks", response_model=PortfolioGreeksResponse)
    def portfolio_greeks(req: PortfolioGreeksRequest) -> PortfolioGreeksResponse:
        started = time.perf_counter()
        try:
            summary = aggregate_portfolio_greeks(
                positions=req.positions,
                repository=repository,
                valuation_time=req.valuation_time,
                vol_overrides=req.vol_overrides,
                enforce_freshness=req.enforce_freshness,
            )
            scenarios = None
            if req.include_scenarios:
                scenarios = run_portfolio_scenarios(
                    positions=req.positions,
                    repository=repository,
                    spot_shocks_pct=req.spot_shocks_pct,
                    vol_shocks_abs=req.vol_shocks_abs,
                    days_forward=req.days_forward,
                    valuation_time=req.valuation_time,
                    enforce_freshness=req.enforce_freshness,
                )
        except (InstrumentNotFoundError, QuoteNotFoundError) as exc:
            raise HTTPException(status_code=404, detail=str(exc)) from exc
        except (StaleQuoteError, ClockSkewError) as exc:
            raise HTTPException(status_code=409, detail=str(exc)) from exc
        except QuoteFeedDisconnectedError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc

        elapsed = time.perf_counter() - started
        metrics_reg.record_risk_calculation(
            latency_seconds=elapsed,
            net_delta=summary.net_delta,
            net_gamma=summary.net_gamma,
            net_vega_per_1pct=summary.net_vega_per_1pct,
            net_theta_per_day=summary.net_theta_per_day,
            worst_case_pnl=scenarios.worst_case_pnl if scenarios else None,
            best_case_pnl=scenarios.best_case_pnl if scenarios else None,
        )
        return PortfolioGreeksResponse(
            summary=summary,
            scenarios=scenarios,
            recalc_latency_ms=round(elapsed * 1000.0, 4),
        )

    @api.post(
        "/v1/strategies/vertical-spread/preview",
        response_model=VerticalSpreadPreview,
    )
    def preview_vertical_spread(req: VerticalSpreadPreviewRequest) -> VerticalSpreadPreview:
        try:
            return build_vertical_spread_preview(
                long_contract_id=req.long_contract_id,
                short_contract_id=req.short_contract_id,
                quantity=req.quantity,
                repository=repository,
                pricing_mode=req.pricing_mode,
                valuation_time=req.valuation_time,
                enforce_freshness=req.enforce_freshness,
            )
        except (InstrumentNotFoundError, QuoteNotFoundError) as exc:
            raise HTTPException(status_code=404, detail=str(exc)) from exc
        except SpreadValidationError as exc:
            raise HTTPException(status_code=422, detail=str(exc)) from exc
        except (StaleQuoteError, ClockSkewError) as exc:
            raise HTTPException(status_code=409, detail=str(exc)) from exc
        except QuoteFeedDisconnectedError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc

    @api.post("/v1/paper/orders", response_model=PaperOrderResponse)
    def submit_paper_order(req: PaperOrderRequest) -> PaperOrderResponse:
        try:
            order, deduplicated = adapter.submit_order(
                legs=req.legs,
                quantity=req.quantity,
                strategy_type=req.strategy_type,
                account_id=req.account_id,
                idempotency_key=req.idempotency_key,
                pricing_mode=req.pricing_mode,
                limit_net_debit_credit=req.limit_net_debit_credit,
                auto_fill=req.auto_fill,
                asof=req.asof or repository.latest_quote_time() or datetime.now(UTC),
            )
        except StoreWriteError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc

        if not deduplicated:
            metrics_reg.record_paper_order(order.status.value)
        return PaperOrderResponse(order=order, deduplicated=deduplicated)

    @api.get("/v1/paper/orders", response_model=list[MultiLegOrder])
    def list_paper_orders(limit: int = Query(default=50, ge=1, le=500)) -> list[MultiLegOrder]:
        return store.list_orders(limit=limit)

    @api.get("/v1/paper/orders/{order_id}", response_model=MultiLegOrder)
    def get_paper_order(order_id: str) -> MultiLegOrder:
        order = store.get_order(order_id)
        if order is None:
            raise HTTPException(status_code=404, detail=f"Unknown paper order: {order_id}")
        return order

    @api.post("/v1/paper/orders/{order_id}/cancel", response_model=MultiLegOrder)
    def cancel_paper_order(order_id: str) -> MultiLegOrder:
        try:
            cancelled = adapter.cancel_order(order_id)
            metrics_reg.record_paper_order(cancelled.status.value)
            return cancelled
        except LookupError as exc:
            raise HTTPException(status_code=404, detail=str(exc)) from exc
        except ValueError as exc:
            raise HTTPException(status_code=409, detail=str(exc)) from exc
        except StoreWriteError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc

    @api.get("/v1/paper/positions", response_model=list[OptionPosition])
    def list_paper_positions(account_id: str = "PAPER-DEFAULT") -> list[OptionPosition]:
        return store.list_positions(account_id=account_id)

    @api.post("/v1/quotes")
    def ingest_quote(quote: OptionQuote) -> dict[str, object]:
        try:
            res = repository.upsert_quote(quote)
            return {
                "accepted": res.accepted,
                "duplicate": res.duplicate,
                "reason": res.reason,
                "warnings": [w.model_dump(mode="json") for w in res.warnings],
            }
        except InstrumentNotFoundError as exc:
            raise HTTPException(status_code=404, detail=str(exc)) from exc
        except QuoteFeedDisconnectedError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc

    return api


app = create_app()
