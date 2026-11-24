"""Integration tests for FastAPI endpoints, metrics, and UI rendering."""

from __future__ import annotations

from fastapi.testclient import TestClient

from options_infra.api.app import create_app


def _client() -> TestClient:
    return TestClient(create_app())


def test_health_ui_and_symbols() -> None:
    client = _client()

    health = client.get("/health")
    assert health.status_code == 200
    body = health.json()
    assert body["status"] == "ok"
    assert body["execution_mode"] == "PAPER_SIMULATED"
    assert "XYZ" in body["symbols"]
    assert "ABC" in body["symbols"]

    home = client.get("/")
    assert home.status_code == 200
    assert "PAPER / SIMULATED ONLY" in home.text

    symbols = client.get("/v1/options/symbols")
    assert symbols.status_code == 200
    assert "2027-01-15" in symbols.json()["XYZ"]


def test_option_chain_price_and_iv_endpoints() -> None:
    client = _client()

    # Exact chain endpoint from README
    chain = client.get("/v1/options/chain?symbol=XYZ&expiry=2027-01-15")
    assert chain.status_code == 200
    data = chain.json()
    assert data["symbol"] == "XYZ"
    assert data["execution_mode"] == "PAPER_SIMULATED"
    assert data["asof"] != data["received_at"]
    assert len(data["rows"]) == 14
    assert all(row["greeks"]["iv"] is not None for row in data["rows"])

    # Exact pricing request payload from README
    price_res = client.post(
        "/v1/options/price",
        json={
            "right": "CALL",
            "spot": 100,
            "strike": 105,
            "years": 0.25,
            "rate": 0.04,
            "dividend_yield": 0.0,
            "volatility": 0.30,
        },
    )
    assert price_res.status_code == 200
    p = price_res.json()
    assert abs(p["price"] - 4.321464) < 1e-4
    assert abs(p["put_call_parity_residual"]) < 1e-11

    # Round-trip IV endpoint
    iv_res = client.post(
        "/v1/options/iv",
        json={
            "market_price": p["price"],
            "right": "CALL",
            "spot": 100,
            "strike": 105,
            "years": 0.25,
            "rate": 0.04,
            "dividend_yield": 0.0,
        },
    )
    assert iv_res.status_code == 200
    iv = iv_res.json()
    assert iv["converged"] is True
    assert abs(iv["implied_volatility"] - 0.30) < 1e-5

    # Binomial CRR American comparison via API
    amer_res = client.post(
        "/v1/options/price",
        json={
            "right": "PUT",
            "spot": 90,
            "strike": 100,
            "years": 1.0,
            "rate": 0.05,
            "dividend_yield": 0.0,
            "volatility": 0.25,
            "style": "AMERICAN",
            "model": "BINOMIAL_CRR",
            "binomial_steps": 150,
        },
    )
    assert amer_res.status_code == 200
    assert amer_res.json()["early_exercise_premium"] > 0.1


def test_vertical_spread_preview_paper_order_portfolio_and_metrics() -> None:
    client = _client()

    # Preview vertical spread
    preview = client.post(
        "/v1/strategies/vertical-spread/preview",
        json={
            "long_contract_id": "XYZ-20270115-100-C",
            "short_contract_id": "XYZ-20270115-105-C",
            "quantity": 2,
            "pricing_mode": "MID",
        },
    )
    assert preview.status_code == 200
    p = preview.json()
    assert p["spread_type"] == "BULL_CALL_DEBIT"
    assert len(p["payoff_curve"]) >= 25
    assert p["assumptions"]["model"] == "BLACK_SCHOLES_MERTON"
    assert len(p["limitations"]) >= 2

    # Submit paper multi-leg order
    order_res = client.post(
        "/v1/paper/orders",
        json={
            "legs": p["legs"],
            "quantity": 2,
            "strategy_type": p["spread_type"],
            "idempotency_key": "api-test-order-1",
            "pricing_mode": "NATURAL",
        },
    )
    assert order_res.status_code == 200
    order_payload = order_res.json()
    assert order_payload["deduplicated"] is False
    assert order_payload["order"]["status"] == "FILLED"
    order_id = order_payload["order"]["id"]

    # Retrieve order and resulting positions
    fetched = client.get(f"/v1/paper/orders/{order_id}")
    assert fetched.status_code == 200
    assert fetched.json()["id"] == order_id

    positions = client.get("/v1/paper/positions").json()
    assert len(positions) == 2

    # Aggregate portfolio Greeks + scenario P&L grid
    risk_res = client.post(
        "/v1/portfolio/greeks",
        json={"positions": positions, "include_scenarios": True},
    )
    assert risk_res.status_code == 200
    risk = risk_res.json()
    assert risk["summary"]["net_delta"] > 0
    assert len(risk["scenarios"]["cells"]) == 21
    assert risk["recalc_latency_ms"] >= 0.0

    # Verify Prometheus & JSON metrics endpoints
    prom = client.get("/metrics")
    assert prom.status_code == 200
    assert "options_iv_solver_convergence_rate" in prom.text
    assert "options_risk_recalc_latency_seconds" in prom.text
    assert "options_portfolio_net_delta" in prom.text

    metrics_json = client.get("/v1/metrics").json()
    assert metrics_json["risk_recalc_samples"] >= 1
    assert metrics_json["iv_solver_convergence_rate"] > 0.0
