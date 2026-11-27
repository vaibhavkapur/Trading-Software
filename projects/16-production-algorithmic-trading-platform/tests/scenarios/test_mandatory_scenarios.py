"""All 8 mandatory failure/scenario tests required by plan.md Section 5."""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from decimal import Decimal
from pathlib import Path

import pytest

from trading_platform.broker.paper_broker import PaperBrokerAdapter
from trading_platform.domain.config import PlatformConfig
from trading_platform.domain.errors import DuplicateOrderConflictError
from trading_platform.domain.events import (
    FeedEntitlement,
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


def _setup_platform(db_path: str = ":memory:") -> tuple[TradingPlatform, PaperBrokerAdapter, MarketTick, datetime]:
    cfg = PlatformConfig.from_json_file(ROOT / "config/platform.paper.json").model_copy(
        update={"db_path": db_path}
    )
    platform = TradingPlatform(cfg)
    assert isinstance(platform.broker, PaperBrokerAdapter)
    now = datetime(2026, 11, 27, 15, 0, 0, tzinfo=UTC)
    platform.register_entitlement(
        FeedEntitlement(
            feed_id="SYNTH-EQUITIES-L1",
            provider="SAAT_SYNTHETIC_GENERATOR",
            license_type="SYNTHETIC_INTERNAL_PERMITTED",
            symbols=["AAPL", "MSFT", "NVDA", "SPY"],
            provenance_uri="data/fixtures/synthetic_paper_feed.json",
            valid_until_utc=datetime(2030, 1, 1, tzinfo=UTC),
        )
    )
    tick = MarketTick(
        tick_id="TK-SCN-001",
        feed_id="SYNTH-EQUITIES-L1",
        symbol="AAPL",
        bid=Decimal("190.00"),
        ask=Decimal("190.04"),
        last=Decimal("190.02"),
        volume=Decimal("1000"),
        sequence_no=1,
        event_time_utc=now - timedelta(milliseconds=25),
        available_time_utc=now - timedelta(milliseconds=10),
    )
    assert platform.process_tick(tick, now_utc=now).accepted
    return platform, platform.broker, tick, now


def _make_intent(
    cid: str,
    idem: str,
    *,
    qty: str = "100",
    limit_px: str = "190.10",
    symbol: str = "AAPL",
    side: Side = Side.BUY,
) -> OrderIntent:
    return OrderIntent(
        idempotency_key=idem,
        client_order_id=cid,
        account_id="ACCT-PAPER-001",
        strategy_id="STRAT-MOM-01",
        symbol=symbol,
        side=side,
        order_type=OrderType.LIMIT,
        quantity=Decimal(qty),
        limit_price=Decimal(limit_px),
        reference_price=Decimal("190.02"),
    )


def test_scenario_1_duplicate_submit() -> None:
    """Duplicate submit returns original order without second broker dispatch; conflicting payload raises."""
    platform, broker, tick, now = _setup_platform()
    intent = _make_intent("ORD-S1-001", "IDEM-S1-001", qty="50")

    o1, _, replay1 = platform.submit_manual_intent(intent, reference_tick=tick, now_utc=now)
    o2, _, replay2 = platform.submit_manual_intent(intent, reference_tick=tick, now_utc=now)

    assert not replay1
    assert replay2
    assert o1.client_order_id == o2.client_order_id
    assert broker.submit_call_count == 1
    assert platform.ledger.positions_map()["AAPL"] == Decimal("50")

    with pytest.raises(DuplicateOrderConflictError):
        platform.submit_manual_intent(
            _make_intent("ORD-S1-002", "IDEM-S1-001", qty="99"),
            reference_tick=tick,
            now_utc=now,
        )
    assert broker.submit_call_count == 1
    platform.close()


def test_scenario_2_unknown_order_outcome_retains_reserved_exposure() -> None:
    """Timeout on submit marks order UNKNOWN_OUTCOME, retains reserved exposure, blocks excess orders, and resolves cleanly on reconcile."""
    platform, broker, tick, now = _setup_platform()
    broker.inject_next_submit_timeout(land_on_broker=True, fill_on_broker=True)

    o_unk, _, _ = platform.submit_manual_intent(
        _make_intent("ORD-S2-001", "IDEM-S2-001", qty="500"),
        reference_tick=tick,
        now_utc=now,
    )
    assert o_unk.state == OrderState.UNKNOWN_OUTCOME

    res = platform.store.get_reservation(o_unk.client_order_id)
    assert res is not None and res.active is True
    assert res.reserved_quantity == Decimal("500")
    assert res.retained_reason is not None and "UNKNOWN_OUTCOME" in res.retained_reason

    # Submit 500 more (reaches 1000 total incl. 500 reserved) -> allowed
    o_ok, _, _ = platform.submit_manual_intent(
        _make_intent("ORD-S2-002", "IDEM-S2-002", qty="500"),
        reference_tick=tick,
        now_utc=now,
    )
    assert o_ok.state == OrderState.FILLED

    # Submit 100 more -> must be blocked because UNKNOWN_OUTCOME retained 500 reserved qty!
    o_blocked, risk_res, _ = platform.submit_manual_intent(
        _make_intent("ORD-S2-003", "IDEM-S2-003", qty="100"),
        reference_tick=tick,
        now_utc=now,
    )
    assert o_blocked.state == OrderState.RISK_REJECTED
    assert risk_res is not None
    assert any("R07_SYMBOL_POSITION_LIMIT" in b for b in risk_res.blocking_reasons)

    # Reconcile resolves unknown order to FILLED, updates ledger, and deactivates reservation
    rec = platform.reconcile(actor="SCENARIO_2")
    assert rec.matched
    assert rec.unknown_orders_resolved == 1
    assert platform.ledger.positions_map()["AAPL"] == Decimal("1000")
    assert len(platform.risk_engine.active_reservations()) == 0
    platform.close()


def test_scenario_3_stale_feed_rejected() -> None:
    """Ticks older than configured max_staleness_ms are rejected and audited without generating orders."""
    platform, broker, _, now = _setup_platform()
    stale = MarketTick(
        tick_id="TK-S3-STALE",
        feed_id="SYNTH-EQUITIES-L1",
        symbol="AAPL",
        bid=Decimal("190.00"),
        ask=Decimal("190.04"),
        last=Decimal("190.02"),
        volume=Decimal("1000"),
        sequence_no=2,
        event_time_utc=now - timedelta(seconds=15),
        available_time_utc=now - timedelta(seconds=10),
    )
    res = platform.process_tick(stale, now_utc=now)
    assert not res.accepted
    assert res.rejection_code == "STALE_FEED"
    assert broker.submit_call_count == 0
    platform.close()


def test_scenario_4_broker_disconnect_and_recovery() -> None:
    """Broker disconnect fails safe to UNKNOWN_OUTCOME with retained reservation and recovers cleanly on reconnect + reconcile."""
    platform, broker, tick, now = _setup_platform()
    broker.set_connected(False)

    order, _, _ = platform.submit_manual_intent(
        _make_intent("ORD-S4-001", "IDEM-S4-001", qty="100"),
        reference_tick=tick,
        now_utc=now,
    )
    assert order.state == OrderState.UNKNOWN_OUTCOME
    res = platform.store.get_reservation("ORD-S4-001")
    assert res is not None and res.active is True

    # Reconnect and reconcile -> broker never received it, so OMS marks REJECTED and releases reserve
    broker.set_connected(True)
    rec = platform.reconcile(actor="SCENARIO_4")
    assert rec.matched
    assert rec.unknown_orders_resolved == 1
    updated = platform.store.get_order("ORD-S4-001")
    res_after = platform.store.get_reservation("ORD-S4-001")
    assert updated is not None and updated.state == OrderState.REJECTED
    assert res_after is not None and res_after.active is False
    platform.close()


def test_scenario_5_cancel_fill_race_full_and_partial() -> None:
    """Cancel/fill race resolves cleanly to FILLED (full race) or CANCELED with partial position intact (partial race)."""
    platform, broker, tick, now = _setup_platform()
    broker.set_resting_mode(True)

    # Full fill wins race against cancel
    o1, _, _ = platform.submit_manual_intent(
        _make_intent("ORD-S5-FULL", "IDEM-S5-FULL", qty="100"),
        reference_tick=tick,
        now_utc=now,
    )
    assert o1.state == OrderState.ACKED
    broker.inject_next_cancel_fill_race(fill_ratio=Decimal("1.0"))
    raced_full = platform.oms.cancel_order(o1.client_order_id, actor="OP", reason="Cancel race")
    assert raced_full.state == OrderState.FILLED
    assert raced_full.filled_quantity == Decimal("100")

    # Partial fill wins race, remainder canceled -> only remaining reserve released
    o2, _, _ = platform.submit_manual_intent(
        _make_intent("ORD-S5-PART", "IDEM-S5-PART", qty="100"),
        reference_tick=tick,
        now_utc=now,
    )
    assert o2.state == OrderState.ACKED
    broker.inject_next_cancel_fill_race(fill_ratio=Decimal("0.4"))
    raced_part = platform.oms.cancel_order(o2.client_order_id, actor="OP", reason="Partial race")
    assert raced_part.state == OrderState.CANCELED
    assert raced_part.filled_quantity == Decimal("40")
    assert platform.ledger.positions_map()["AAPL"] == Decimal("140")
    assert len(platform.risk_engine.active_reservations()) == 0
    assert platform.reconcile(actor="SCENARIO_5").matched
    platform.close()


def test_scenario_6_restart_recovery_preserves_state_and_reconciles(tmp_path: Path) -> None:
    """Crash/restart reloads durable SQLite state, retains kill switches, preserves idempotency, and reconciles uncertain orders."""
    db_file = str(tmp_path / "restart.db")
    p1, broker, tick, now = _setup_platform(db_path=db_file)
    broker.inject_next_submit_timeout(land_on_broker=True, fill_on_broker=True)

    o_unk, _, _ = p1.submit_manual_intent(
        _make_intent("ORD-S6-001", "IDEM-S6-001", qty="120"),
        reference_tick=tick,
        now_utc=now,
    )
    assert o_unk.state == OrderState.UNKNOWN_OUTCOME
    p1.engage_kill_switch(
        scope=KillSwitchScope.STRATEGY,
        target="STRAT-MR-01",
        reason="Persist across restart",
        actor="op-1",
        actor_role=Role.OPERATOR,
    )
    p1.close()

    # Fresh process restart with same SQLite DB & broker
    cfg2 = PlatformConfig.from_json_file(ROOT / "config/platform.paper.json").model_copy(
        update={"db_path": db_file}
    )
    p2 = TradingPlatform(cfg2, broker=broker)
    recovery = p2.recover_after_restart()
    assert recovery["audit_chain_valid"] is True
    assert recovery["ledger_balanced"] is True
    assert recovery["reconciled"] is True
    assert recovery["unknown_orders_resolved"] == 1

    # Idempotency key survived restart
    replayed, _, was_replay = p2.submit_manual_intent(
        _make_intent("ORD-S6-001", "IDEM-S6-001", qty="120"),
        reference_tick=tick,
        now_utc=now,
    )
    assert was_replay and replayed.state == OrderState.FILLED

    # Kill switch survived restart
    active_ks = p2.store.list_kill_switches(engaged_only=True)
    assert len(active_ks) == 1 and active_ks[0].target == "STRAT-MR-01"
    p2.close()


def test_scenario_7_limit_breach_blocks_before_broker_dispatch() -> None:
    """Fat-finger collar, max order qty, max order notional, and disallowed symbol are all blocked before broker dispatch."""
    platform, broker, tick, now = _setup_platform()

    cases = [
        ("R03_SYMBOL_ALLOWLIST", _make_intent("O-SYM", "I-SYM", symbol="TSLA", qty="10")),
        ("R04_PRICE_COLLAR", _make_intent("O-FF", "I-FF", qty="10", limit_px="220.00")),
        ("R05_MAX_ORDER_QTY", _make_intent("O-QTY", "I-QTY", qty="550", limit_px="190.10")),
        ("R06_MAX_ORDER_NOTIONAL", _make_intent("O-NOT", "I-NOT", qty="500", limit_px="205.00")),
    ]
    for expected_rule, intent in cases:
        order, risk_res, _ = platform.submit_manual_intent(
            intent, reference_tick=tick, now_utc=now
        )
        assert order.state == OrderState.RISK_REJECTED
        assert risk_res is not None
        assert any(expected_rule in reason for reason in risk_res.blocking_reasons)

    assert broker.submit_call_count == 0
    platform.close()


def test_scenario_8_clock_skew_quarantined() -> None:
    """Forward wall-clock skew and inverted event_time > available_time are rejected with CLOCK_SKEW_DETECTED."""
    platform, broker, _, now = _setup_platform()

    future_tick = MarketTick(
        tick_id="TK-S8-FUTURE",
        feed_id="SYNTH-EQUITIES-L1",
        symbol="AAPL",
        bid=Decimal("190.00"),
        ask=Decimal("190.04"),
        last=Decimal("190.02"),
        volume=Decimal("500"),
        sequence_no=2,
        event_time_utc=now + timedelta(seconds=10),
        available_time_utc=now + timedelta(seconds=10),
    )
    res1 = platform.process_tick(future_tick, now_utc=now)
    assert not res1.accepted and res1.rejection_code == "CLOCK_SKEW_DETECTED"

    inverted_tick = MarketTick(
        tick_id="TK-S8-INV",
        feed_id="SYNTH-EQUITIES-L1",
        symbol="AAPL",
        bid=Decimal("190.00"),
        ask=Decimal("190.04"),
        last=Decimal("190.02"),
        volume=Decimal("500"),
        sequence_no=3,
        event_time_utc=now - timedelta(milliseconds=10),
        available_time_utc=now - timedelta(milliseconds=500),
    )
    res2 = platform.process_tick(inverted_tick, now_utc=now)
    assert not res2.accepted and res2.rejection_code == "CLOCK_SKEW_DETECTED"
    assert broker.submit_call_count == 0
    platform.close()
