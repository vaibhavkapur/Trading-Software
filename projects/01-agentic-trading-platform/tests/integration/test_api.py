"""HTTP contract tests against the real app, store and simulator."""

import pytest
from conftest import make_service
from fastapi.testclient import TestClient

from atp.api import create_app
from atp.config import DEV_API_KEY

AUTH = {"Authorization": f"Bearer {DEV_API_KEY}"}


@pytest.fixture
def client(clock):
    svc = make_service(clock)
    with TestClient(create_app(service=svc), headers=AUTH) as c:
        c.svc = svc
        c.clock = clock
        yield c


def propose(client, text="Buy $500 of SPY using a limit order"):
    r = client.post("/v1/intents", json={"text": text, "mode": "paper"})
    assert r.status_code == 201, r.text
    return r.json()


def approve(client, proposal):
    r = client.post(f"/v1/proposals/{proposal['id']}/evaluate")
    assert r.json()["risk_decision"]["passed"], r.text
    r = client.post(
        f"/v1/proposals/{proposal['id']}/approve",
        json={"proposal_hash": proposal["proposal_hash"], "decision": "approve"},
    )
    assert r.status_code == 200, r.text
    return r.json()["order_id"]


def test_readme_flow_end_to_end(client):
    created = propose(client)
    proposal, cid = created["proposal"], created["correlation_id"]
    assert proposal["status"] == "draft" and proposal["quantity"] == 3
    assert proposal["proposal_hash"].startswith("sha256:")
    assert created["intent"]["planner"] == "rules"

    order_id = approve(client, proposal)
    view = client.get(f"/v1/proposals/{proposal['id']}").json()
    assert view["proposal"]["status"] == "approved" and view["order_id"] == order_id
    assert "signature" not in view["approval"]

    r = client.post(f"/v1/orders/{order_id}/submit")
    assert r.status_code == 200
    assert r.json()["order"]["state"] == "filled"
    assert r.json()["order"]["filled_qty"] == 3

    r = client.get(f"/v1/orders/{order_id}")
    assert r.json()["order"]["state"] == "filled" and len(r.json()["fills"]) == 1

    portfolio = client.get("/v1/portfolio").json()
    assert portfolio["mode"] == "paper"
    assert portfolio["positions"] == [{"symbol": "SPY", "quantity": 3, "avg_cost": "125.2500"}]
    assert portfolio["cash"] == "99624.25"  # money is serialised as strings, never floats

    events = client.get("/v1/audit", params={"correlation_id": cid}).json()["events"]
    types = [e["event_type"] for e in events]
    assert types[0] == "tool.called" and "order.fill" in types
    assert client.get("/v1/audit/verify").json() == {"chain_intact": True, "first_bad_seq": None}


def test_caller_supplied_correlation_id_is_propagated(client):
    r = client.post(
        "/v1/intents", json={"text": "Buy $500 of SPY"}, headers={"X-Correlation-ID": "trace-42"}
    )
    assert r.headers["X-Correlation-ID"] == "trace-42"
    events = client.get("/v1/audit", params={"correlation_id": "trace-42"}).json()["events"]
    assert events and all(e["correlation_id"] == "trace-42" for e in events)


def test_authentication_is_required(client):
    for headers in (
        {"Authorization": ""},
        {"Authorization": "Bearer wrong"},
        {"Authorization": "Basic abc"},
    ):
        for method, path in (
            ("get", "/v1/portfolio"),
            ("post", "/v1/intents"),
            ("get", "/v1/audit?correlation_id=x"),
            ("post", "/v1/sim/faults"),
        ):
            r = getattr(client, method)(path, headers=headers)
            assert r.status_code == 401, (path, headers)


def test_error_contract(client):
    r = client.post("/v1/intents", json={"text": "Buy $500 of SPY", "mode": "live"})
    assert r.status_code == 400 and r.json()["error"]["code"] == "MODE_NOT_SUPPORTED"

    r = client.post("/v1/intents", json={"text": "how is the market today?"})
    assert r.status_code == 422 and r.json()["error"]["code"] == "INTENT_NOT_ACTIONABLE"

    r = client.post("/v1/intents", json={"text": "Buy $500 of GME"})
    assert r.status_code == 422 and r.json()["error"]["code"] == "SYMBOL_NOT_ALLOWED"

    assert client.get("/v1/proposals/prp_missing").status_code == 404
    assert client.post("/v1/orders/ord_missing/submit").status_code == 404
    assert client.post("/v1/intents", json={"text": ""}).status_code == 422

    proposal = propose(client)["proposal"]
    r = client.post(
        f"/v1/proposals/{proposal['id']}/approve",
        json={"proposal_hash": proposal["proposal_hash"], "decision": "approve"},
    )
    assert r.status_code == 409 and r.json()["error"]["code"] == "RISK_NOT_PASSED"

    client.post(f"/v1/proposals/{proposal['id']}/evaluate")
    r = client.post(
        f"/v1/proposals/{proposal['id']}/approve",
        json={"proposal_hash": "sha256:bogus", "decision": "approve"},
    )
    assert r.status_code == 409 and r.json()["error"]["code"] == "PROPOSAL_HASH_MISMATCH"


def test_risk_rejection_lists_structured_reasons(client):
    proposal = propose(client, "Buy 5000 shares of SPY")["proposal"]
    body = client.post(f"/v1/proposals/{proposal['id']}/evaluate").json()
    decision = body["risk_decision"]
    assert decision["passed"] is False and decision["policy_version"]
    codes = {v["code"] for v in decision["violations"]}
    assert codes == {"MAX_ORDER_NOTIONAL", "MAX_POSITION_NOTIONAL", "INSUFFICIENT_BUYING_POWER"}
    assert all(v["detail"] for v in decision["violations"])
    assert body["proposal"]["status"] == "risk_rejected"


def test_duplicate_submit_over_http(client):
    order_id = approve(client, propose(client)["proposal"])
    bodies = [client.post(f"/v1/orders/{order_id}/submit").json() for _ in range(3)]
    assert bodies[0] == bodies[1] == bodies[2]
    assert len(client.svc.db.all("SELECT * FROM broker_orders")) == 1


def test_fault_injection_expired_approval_and_stale_quote(client):
    order_id = approve(client, propose(client)["proposal"])
    client.clock.advance(client.svc.settings.approval_ttl_seconds + 1)
    order = client.post(f"/v1/orders/{order_id}/submit").json()["order"]
    assert (order["state"], order["reject_reason"]) == ("rejected", "APPROVAL_EXPIRED")

    order_id = approve(client, propose(client, "Buy $700 of QQQ")["proposal"])
    assert client.post("/v1/sim/faults", json={"quotes_stalled": True}).json()["quotes_stalled"]
    client.clock.advance(10)
    order = client.post(f"/v1/orders/{order_id}/submit").json()["order"]
    assert (order["state"], order["reject_reason"]) == ("rejected", "RISK:QUOTE_STALE")
    assert client.get("/v1/portfolio").json()["positions"] == []


def test_broker_outage_returns_retryable_503_then_recovers(client):
    order_id = approve(client, propose(client)["proposal"])
    client.post("/v1/sim/faults", json={"broker_order_entry_down": True})
    r = client.post(f"/v1/orders/{order_id}/submit")
    assert r.status_code == 503 and r.headers["Retry-After"]
    assert r.json()["error"] == {
        "code": "BROKER_UNAVAILABLE",
        "message": r.json()["error"]["message"],
        "retryable": True,
    }
    assert r.json()["order"]["state"] == "submitting" and r.json()["order"]["filled_qty"] == 0

    client.post("/v1/sim/faults", json={"broker_order_entry_down": False})
    assert client.post(f"/v1/orders/{order_id}/submit").json()["order"]["state"] == "filled"
    assert len(client.svc.db.all("SELECT * FROM broker_orders")) == 1


def test_metrics_are_exposed(client):
    order_id = approve(client, propose(client)["proposal"])
    client.post(f"/v1/orders/{order_id}/submit")
    client.post(f"/v1/orders/{order_id}/submit")
    rejected = propose(client, "Buy 5000 shares of SPY")["proposal"]
    client.post(f"/v1/proposals/{rejected['id']}/evaluate")
    text = client.get("/metrics").text
    assert 'atp_order_submissions_total{outcome="accepted"} 1.0' in text
    assert 'atp_order_submissions_total{outcome="duplicate"} 1.0' in text
    assert (
        'atp_policy_rejections_total{reason="MAX_ORDER_NOTIONAL",stage="pre_approval"} 1.0' in text
    )
    assert 'atp_proposal_to_approval_seconds_count{decision="approve"} 1.0' in text
    assert "atp_quote_staleness_seconds_bucket" in text
    assert "atp_order_submit_retries_total" in text
    assert "atp_order_reconciliation_mismatch_total" in text


def test_ui_and_health(client):
    assert client.get("/healthz").json() == {"status": "ok", "mode": "paper", "planner": "rules"}
    page = client.get("/")
    assert page.status_code == 200 and "PAPER" in page.text
    assert DEV_API_KEY not in page.text  # no credentials in browser code
    assert {t["scope"] for t in client.get("/v1/tools").json()["tools"]} == {"read"}


def test_fault_endpoints_absent_outside_dev(clock):
    svc = make_service(clock, env="prod", api_keys={"k": "alice"})
    with TestClient(create_app(service=svc), headers={"Authorization": "Bearer k"}) as c:
        assert c.post("/v1/sim/faults", json={"quotes_down": True}).status_code == 404
        assert c.get("/v1/portfolio").status_code == 200
