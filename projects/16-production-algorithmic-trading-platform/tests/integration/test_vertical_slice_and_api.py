"""Integration tests for vertical slice, broker fixture reconciliation, shadow mode, kill switch, and API RBAC."""

from __future__ import annotations

import json
from datetime import UTC, datetime, timedelta
from decimal import Decimal
from pathlib import Path

import pytest

from trading_platform.api.server import OperatorAPIRouter
from trading_platform.broker.paper_broker import PaperBrokerAdapter
from trading_platform.domain.config import PlatformConfig
from trading_platform.domain.errors import AuthorizationError
from trading_platform.domain.events import (
    BrokerAccountSnapshot,
    FeedEntitlement,
    FillEvent,
    KillSwitchScope,
    MarketTick,
    OrderIntent,
    OrderState,
    OrderType,
    Role,
    Side,
)
from trading_platform.platform import TradingPlatform

ROOT = Path(__file__).resolve().parent.parent.parent


def test_ledger_reconciles_to_broker_fixture_and_detects_drift() -> None:
    fixture_raw = json.loads(
        (ROOT / "data/fixtures/broker_reconciliation_fixture.json").read_text(encoding="utf-8")
    )
    broker_snapshot = BrokerAccountSnapshot.model_validate(fixture_raw)

    cfg = PlatformConfig(account_id=broker_snapshot.account_id, db_path=":memory:")
    platform = TradingPlatform(cfg)

    # Book matching order + fill locally through OMS/ledger
    intent = OrderIntent(
        idempotency_key="IDEM-FIX-RECON-001",
        client_order_id="ORD-FIX-RECON-001",
        account_id=broker_snapshot.account_id,
        strategy_id="STRAT-MOM-01",
        symbol="AAPL",
        side=Side.BUY,
        order_type=OrderType.LIMIT,
        quantity=Decimal("50"),
        limit_price=Decimal("191.50"),
        reference_price=Decimal("191.02"),
    )
    # Simulate order timed out at submit so reconciler resolves from broker fixture directly!
    assert isinstance(platform.broker, PaperBrokerAdapter)
    platform.broker.inject_next_submit_timeout(land_on_broker=False, fill_on_broker=False)
    order, _, _ = platform.submit_manual_intent(intent)
    assert order.state == OrderState.UNKNOWN_OUTCOME

    # Reconcile directly against external broker fixture
    report = platform.reconcile(broker_snapshot=broker_snapshot, actor="FIXTURE_TEST")
    assert report.matched
    assert report.unknown_orders_resolved == 1
    assert report.cash_delta == Decimal("0.0000")
    assert report.local_positions == {"AAPL": Decimal("50")}
    assert platform.ledger.is_balanced()

    # Now inject cash drift on fixture and verify critical break engages kill switch
    drifted = broker_snapshot.model_copy(update={"cash_balance": Decimal("990000.0000")})
    bad_report = platform.reconcile(broker_snapshot=drifted, actor="DRIFT_TEST")
    assert not bad_report.matched
    assert bad_report.kill_switch_triggered
    assert len(platform.store.list_kill_switches(engaged_only=True)) == 1
    platform.close()


def test_kill_switch_blocks_dispatch_and_enforces_dual_release() -> None:
    cfg = PlatformConfig.from_json_file(ROOT / "config/platform.paper.json").model_copy(
        update={"db_path": ":memory:"}
    )
    platform = TradingPlatform(cfg)
    assert isinstance(platform.broker, PaperBrokerAdapter)

    ks, _ = platform.engage_kill_switch(
        scope=KillSwitchScope.GLOBAL,
        target="ALL",
        reason="Critical operational anomaly",
        actor="op-1",
        actor_role=Role.OPERATOR,
        requires_dual_release=True,
    )
    assert ks.engaged

    intent = OrderIntent(
        idempotency_key="IDEM-KS-1",
        client_order_id="ORD-KS-1",
        account_id=cfg.account_id,
        strategy_id="STRAT-MOM-01",
        symbol="AAPL",
        side=Side.BUY,
        order_type=OrderType.LIMIT,
        quantity=Decimal("10"),
        limit_price=Decimal("190.00"),
        reference_price=Decimal("190.00"),
    )
    order, rchk, _ = platform.submit_manual_intent(intent)
    assert order.state == OrderState.RISK_REJECTED
    assert rchk is not None and any("R02_KILL_SWITCH" in r for r in rchk.blocking_reasons)
    assert platform.broker.submit_call_count == 0

    # Single release fails when requires_dual_release=True
    with pytest.raises(AuthorizationError):
        platform.release_kill_switch(
            reason="Attempt single release",
            actor="op-1",
            actor_role=Role.OPERATOR,
        )

    # Same actor as secondary fails maker-checker
    with pytest.raises(AuthorizationError):
        platform.release_kill_switch(
            reason="Self approve",
            actor="risk-1",
            actor_role=Role.RISK_OFFICER,
            secondary_approver="risk-1",
            secondary_role=Role.RISK_OFFICER,
        )

    # Proper dual release succeeds
    released = platform.release_kill_switch(
        reason="Verified clean recovery",
        actor="op-1",
        actor_role=Role.OPERATOR,
        secondary_approver="risk-1",
        secondary_role=Role.RISK_OFFICER,
    )
    assert not released.engaged
    platform.close()


def test_shadow_mode_evaluates_entitled_feed_with_zero_broker_dispatch() -> None:
    cfg = PlatformConfig.from_json_file(ROOT / "config/platform.shadow.json").model_copy(
        update={"db_path": ":memory:"}
    )
    platform = TradingPlatform(cfg)
    assert isinstance(platform.broker, PaperBrokerAdapter)
    platform.register_entitlement(
        FeedEntitlement(
            feed_id="ENTITLED-DELAYED-SHADOW-L1",
            provider="SAAT_SANDBOX_FEED_SIM",
            license_type="PERMITTED_SHADOW_EVALUATION",
            allowed_modes=[cfg.execution_policy.execution_mode],
            symbols=["AAPL", "MSFT"],
            provenance_uri="data/fixtures/entitled_shadow_feed.json",
            valid_until_utc=datetime(2030, 1, 1, tzinfo=UTC),
        )
    )

    raw = json.loads(
        (ROOT / "data/fixtures/entitled_shadow_feed.json").read_text(encoding="utf-8")
    )
    base = datetime(2026, 11, 27, 15, 0, 0, tzinfo=UTC)
    for item in raw["ticks"]:
        ts = base + timedelta(milliseconds=int(item["offset_ms"]))
        tick = MarketTick(
            tick_id=item["tick_id"],
            feed_id=raw["feed_id"],
            symbol=item["symbol"],
            bid=Decimal(item["bid"]),
            ask=Decimal(item["ask"]),
            last=Decimal(item["last"]),
            volume=Decimal(item["volume"]),
            sequence_no=int(item["sequence_no"]),
            event_time_utc=ts,
            available_time_utc=ts + timedelta(milliseconds=5),
        )
        res = platform.process_tick(tick, now_utc=ts + timedelta(milliseconds=10))
        assert res.accepted

    decisions = platform.store.list_shadow_decisions()
    assert len(decisions) >= 1
    assert all(not d.dispatched_to_broker for d in decisions)
    assert platform.broker.submit_call_count == 0
    assert len(platform.store.list_orders()) == 0
    platform.close()


def test_operator_api_rbac_and_audit_chain() -> None:
    cfg = PlatformConfig.from_json_file(ROOT / "config/platform.paper.json").model_copy(
        update={"db_path": ":memory:"}
    )
    platform = TradingPlatform(cfg)
    router = OperatorAPIRouter(platform)

    code, health = router.handle("GET", "/api/health")
    assert code == 200 and health["status"] == "ok"

    # VIEWER cannot engage kill switch
    code, denied = router.handle(
        "POST",
        "/api/kill-switch/engage",
        body={"reason": "test"},
        headers={"X-Actor-Id": "viewer-1", "X-Actor-Role": "VIEWER"},
    )
    assert code == 403 and denied["error_code"] == "AUTHORIZATION_DENIED"

    # RISK_OFFICER can engage and release
    code, engaged = router.handle(
        "POST",
        "/api/kill-switch/engage",
        body={"reason": "API drill"},
        headers={"X-Actor-Id": "ro-1", "X-Actor-Role": "RISK_OFFICER"},
    )
    assert code == 200 and engaged["kill_switch"]["engaged"]

    code, audit = router.handle("GET", "/api/audit")
    assert code == 200 and audit["valid"] is True

    # Ensure duplicate fill replay via OMS is idempotent
    fill = FillEvent(
        fill_id="FIL-API-1",
        client_order_id="MISSING",
        broker_order_id="B1",
        account_id=cfg.account_id,
        strategy_id="S1",
        symbol="AAPL",
        side=Side.BUY,
        quantity=Decimal("10"),
        price=Decimal("100"),
    )
    assert platform.store.save_fill_if_absent(fill) is True
    assert platform.store.save_fill_if_absent(fill) is False
    platform.close()
