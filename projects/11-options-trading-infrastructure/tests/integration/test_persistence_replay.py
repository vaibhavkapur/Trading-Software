"""Integration tests for SQLite restart persistence and deterministic fixture replay."""

from __future__ import annotations

from pathlib import Path

from fastapi.testclient import TestClient

from options_infra.api.app import create_app
from options_infra.instruments.repository import InstrumentRepository
from options_infra.instruments.simulator import (
    DEFAULT_ASOF,
    generate_default_universe,
    save_fixture_file,
)
from options_infra.paper_adapter.store import PaperOrderStore


def test_sqlite_persistence_across_restart(tmp_path: Path) -> None:
    db_file = tmp_path / "paper_orders.sqlite3"

    # First instance: submit and fill a vertical spread paper order
    store1 = PaperOrderStore(db_path=db_file)
    app1 = create_app(store=store1)
    client1 = TestClient(app1)

    res = client1.post(
        "/v1/paper/orders",
        json={
            "legs": [
                {"contract_id": "XYZ-20270115-100-C", "side": "BUY", "ratio": 1},
                {"contract_id": "XYZ-20270115-105-C", "side": "SELL", "ratio": 1},
            ],
            "quantity": 3,
            "strategy_type": "BULL_CALL_DEBIT",
            "idempotency_key": "restart-persist-001",
        },
    )
    assert res.status_code == 200
    order_id = res.json()["order"]["id"]
    store1.close()

    # Second instance after restart: order, idempotency, and positions are preserved
    store2 = PaperOrderStore(db_path=db_file)
    app2 = create_app(store=store2)
    client2 = TestClient(app2)

    fetched = client2.get(f"/v1/paper/orders/{order_id}")
    assert fetched.status_code == 200
    assert fetched.json()["status"] == "FILLED"

    # Retrying with same idempotency key after restart deduplicates cleanly
    retry = client2.post(
        "/v1/paper/orders",
        json={
            "legs": [
                {"contract_id": "XYZ-20270115-100-C", "side": "BUY", "ratio": 1},
                {"contract_id": "XYZ-20270115-105-C", "side": "SELL", "ratio": 1},
            ],
            "quantity": 3,
            "strategy_type": "BULL_CALL_DEBIT",
            "idempotency_key": "restart-persist-001",
        },
    )
    assert retry.status_code == 200
    assert retry.json()["deduplicated"] is True
    assert retry.json()["order"]["id"] == order_id

    positions = {p["contract_id"]: p["quantity"] for p in client2.get("/v1/paper/positions").json()}
    assert positions == {"XYZ-20270115-100-C": 3, "XYZ-20270115-105-C": -3}
    store2.close()


def test_deterministic_fixture_replay_reproducibility(tmp_path: Path) -> None:
    fixture_path = tmp_path / "replay_fixture.json"
    contracts, quotes = generate_default_universe(asof=DEFAULT_ASOF)
    save_fixture_file(fixture_path, contracts, quotes)

    def run_once() -> tuple[dict, dict]:
        repo = InstrumentRepository.from_fixture(fixture_path)
        client = TestClient(create_app(repository=repo))
        chain = client.get("/v1/options/chain?symbol=XYZ&expiry=2027-01-15").json()
        preview = client.post(
            "/v1/strategies/vertical-spread/preview",
            json={
                "long_contract_id": "XYZ-20270115-100-C",
                "short_contract_id": "XYZ-20270115-105-C",
                "quantity": 2,
                "pricing_mode": "MID",
            },
        ).json()
        return chain, preview

    chain1, preview1 = run_once()
    chain2, preview2 = run_once()
    assert chain1 == chain2
    assert preview1 == preview2
