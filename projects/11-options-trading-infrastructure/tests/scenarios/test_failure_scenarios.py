"""Failure and degradation scenario tests.

Covers every failure mode required by the specification:
- Missing contract / missing quote data
- Clock skew between venue `asof` and local `received_at`
- Stale quotes
- Out-of-order quote events
- Market-data feed disconnect and recovery
- Duplicate delivery (quotes & multi-leg orders)
- Rejected storage writes
"""

from __future__ import annotations

from datetime import date, timedelta
from decimal import Decimal

from fastapi.testclient import TestClient

from options_infra.api.app import create_app
from options_infra.domain.models import ExerciseStyle, OptionContract, OptionQuote, OptionRight
from options_infra.instruments.repository import InstrumentRepository
from options_infra.instruments.simulator import DEFAULT_ASOF
from options_infra.paper_adapter.store import PaperOrderStore


def test_missing_contract_and_missing_quote() -> None:
    repo = InstrumentRepository.from_default_simulator(asof=DEFAULT_ASOF)
    # Add contract with NO quote
    repo.upsert_contract(
        OptionContract(
            id="XYZ-20270115-125-C",
            underlying="XYZ",
            expiry=date(2027, 1, 15),
            strike=Decimal("125.00"),
            right=OptionRight.CALL,
            style=ExerciseStyle.EUROPEAN,
        )
    )
    client = TestClient(create_app(repository=repo))

    # Unknown chain returns 404
    assert client.get("/v1/options/chain?symbol=NOPE&expiry=2027-01-15").status_code == 404

    # Preview referencing contract with missing quote returns 404
    res = client.post(
        "/v1/strategies/vertical-spread/preview",
        json={
            "long_contract_id": "XYZ-20270115-100-C",
            "short_contract_id": "XYZ-20270115-125-C",
            "quantity": 1,
        },
    )
    assert res.status_code == 404

    # Paper order referencing contract with missing quote is safely REJECTED
    order_res = client.post(
        "/v1/paper/orders",
        json={
            "legs": [
                {"contract_id": "XYZ-20270115-100-C", "side": "BUY", "ratio": 1},
                {"contract_id": "XYZ-20270115-125-C", "side": "SELL", "ratio": 1},
            ],
            "quantity": 1,
        },
    )
    assert order_res.status_code == 200
    assert order_res.json()["order"]["status"] == "REJECTED"
    assert "MARKET_DATA_UNAVAILABLE" in order_res.json()["order"]["rejection_reason"]


def test_stale_quote_blocks_execution_and_strict_risk() -> None:
    repo = InstrumentRepository.from_default_simulator(
        asof=DEFAULT_ASOF,
        max_quote_age_seconds=60.0,
    )
    client = TestClient(create_app(repository=repo))
    future_now = (DEFAULT_ASOF + timedelta(minutes=15)).isoformat()

    # Strict portfolio Greeks with enforce_freshness=True rejects stale quotes (409)
    risk = client.post(
        "/v1/portfolio/greeks",
        json={
            "positions": [
                {"contract_id": "XYZ-20270115-100-C", "quantity": 1, "avg_price": "6.00"}
            ],
            "valuation_time": future_now,
            "enforce_freshness": True,
        },
    )
    assert risk.status_code == 409
    assert "stale" in risk.json()["detail"].lower()

    # Paper order evaluated against stale quotes is REJECTED by pre-trade risk gate
    order = client.post(
        "/v1/paper/orders",
        json={
            "legs": [
                {"contract_id": "XYZ-20270115-100-C", "side": "BUY", "ratio": 1},
                {"contract_id": "XYZ-20270115-105-C", "side": "SELL", "ratio": 1},
            ],
            "quantity": 1,
            "asof": future_now,
        },
    )
    assert order.status_code == 200
    body = order.json()["order"]
    assert body["status"] == "REJECTED"
    assert "data-freshness guard" in body["rejection_reason"]


def test_clock_skew_and_out_of_order_quote_handling() -> None:
    repo = InstrumentRepository.from_default_simulator(
        asof=DEFAULT_ASOF,
        max_clock_skew_seconds=10.0,
    )
    client = TestClient(create_app(repository=repo))

    # Out-of-order quote (earlier venue asof) is dropped safely
    old_quote = OptionQuote(
        contract_id="XYZ-20270115-100-C",
        bid=Decimal("5.00"),
        ask=Decimal("5.10"),
        underlying_px=Decimal("100.00"),
        asof=DEFAULT_ASOF - timedelta(seconds=30),
        received_at=DEFAULT_ASOF + timedelta(seconds=1),
        sequence_id=5001,
    )
    drop_res = client.post("/v1/quotes", json=old_quote.model_dump(mode="json"))
    assert drop_res.status_code == 200
    assert drop_res.json()["accepted"] is False
    assert "Out-of-order" in drop_res.json()["reason"]

    # Skewed quote (venue timestamp 90s ahead of local receive clock) triggers warning & blocks order
    skewed_quote = OptionQuote(
        contract_id="XYZ-20270115-100-C",
        bid=Decimal("6.00"),
        ask=Decimal("6.20"),
        underlying_px=Decimal("100.00"),
        asof=DEFAULT_ASOF + timedelta(seconds=95),
        received_at=DEFAULT_ASOF + timedelta(seconds=5),
        sequence_id=5002,
    )
    ingest = client.post("/v1/quotes", json=skewed_quote.model_dump(mode="json"))
    assert ingest.status_code == 200
    assert ingest.json()["accepted"] is True
    assert any(w["code"] == "CLOCK_SKEW_DETECTED" for w in ingest.json()["warnings"])

    order = client.post(
        "/v1/paper/orders",
        json={
            "legs": [
                {"contract_id": "XYZ-20270115-100-C", "side": "BUY", "ratio": 1},
                {"contract_id": "XYZ-20270115-105-C", "side": "SELL", "ratio": 1},
            ],
            "quantity": 1,
            "asof": (DEFAULT_ASOF + timedelta(seconds=96)).isoformat(),
        },
    )
    assert order.status_code == 200
    assert order.json()["order"]["status"] == "REJECTED"
    assert "clock-skew guard" in order.json()["order"]["rejection_reason"]


def test_feed_disconnect_and_clean_recovery() -> None:
    repo = InstrumentRepository.from_default_simulator(asof=DEFAULT_ASOF)
    client = TestClient(create_app(repository=repo))

    repo.set_feed_connected(False)
    assert client.get("/health").json()["status"] == "degraded"
    assert client.get("/v1/options/chain?symbol=XYZ&expiry=2027-01-15").status_code == 503

    rejected = client.post(
        "/v1/paper/orders",
        json={
            "legs": [
                {"contract_id": "XYZ-20270115-100-C", "side": "BUY", "ratio": 1},
                {"contract_id": "XYZ-20270115-105-C", "side": "SELL", "ratio": 1},
            ],
            "quantity": 1,
            "asof": DEFAULT_ASOF.isoformat(),
        },
    )
    assert rejected.status_code == 200
    assert rejected.json()["order"]["status"] == "REJECTED"
    assert "MARKET_DATA_UNAVAILABLE" in rejected.json()["order"]["rejection_reason"]

    # Reconnect restores chain and paper orders cleanly
    repo.set_feed_connected(True)
    assert client.get("/health").json()["status"] == "ok"
    assert client.get("/v1/options/chain?symbol=XYZ&expiry=2027-01-15").status_code == 200


def test_duplicate_quote_and_duplicate_order_idempotency() -> None:
    repo = InstrumentRepository.from_default_simulator(asof=DEFAULT_ASOF)
    client = TestClient(create_app(repository=repo))

    q = repo.get_quote("XYZ-20270115-100-C")
    dup_quote = client.post("/v1/quotes", json=q.model_dump(mode="json"))
    assert dup_quote.status_code == 200
    assert dup_quote.json()["duplicate"] is True
    assert dup_quote.json()["accepted"] is False

    payload = {
        "legs": [
            {"contract_id": "XYZ-20270115-100-C", "side": "BUY", "ratio": 1},
            {"contract_id": "XYZ-20270115-105-C", "side": "SELL", "ratio": 1},
        ],
        "quantity": 2,
        "idempotency_key": "dup-delivery-key-42",
        "asof": DEFAULT_ASOF.isoformat(),
    }
    first = client.post("/v1/paper/orders", json=payload).json()
    second = client.post("/v1/paper/orders", json=payload).json()
    assert first["deduplicated"] is False
    assert second["deduplicated"] is True
    assert first["order"]["id"] == second["order"]["id"]

    # Ensure positions were NOT double-counted
    positions = {p["contract_id"]: p["quantity"] for p in client.get("/v1/paper/positions").json()}
    assert positions == {"XYZ-20270115-100-C": 2, "XYZ-20270115-105-C": -2}


def test_rejected_storage_write_returns_503_without_partial_state() -> None:
    store = PaperOrderStore(":memory:")
    app = create_app(store=store)
    client = TestClient(app)

    store.set_simulate_write_failure(True)
    failed = client.post(
        "/v1/paper/orders",
        json={
            "legs": [
                {"contract_id": "XYZ-20270115-100-C", "side": "BUY", "ratio": 1},
                {"contract_id": "XYZ-20270115-105-C", "side": "SELL", "ratio": 1},
            ],
            "quantity": 1,
            "idempotency_key": "write-fail-001",
            "asof": DEFAULT_ASOF.isoformat(),
        },
    )
    assert failed.status_code == 503
    assert "write failure" in failed.json()["detail"].lower()

    # Re-enabling storage leaves zero phantom orders or positions
    store.set_simulate_write_failure(False)
    assert client.get("/v1/paper/orders").json() == []
    assert client.get("/v1/paper/positions").json() == []
