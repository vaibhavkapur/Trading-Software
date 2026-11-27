#!/usr/bin/env python3
"""Executes all 8 mandatory fault/failure drills from plan.md Section 5 and retains raw JSON evidence."""

from __future__ import annotations

import json
import sys
import tempfile
from datetime import UTC, datetime, timedelta
from decimal import Decimal
from pathlib import Path
from typing import Any

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

ROOT = Path(__file__).resolve().parent.parent


def _base_platform(db_path: str = ":memory:") -> tuple[TradingPlatform, PaperBrokerAdapter, MarketTick, datetime]:
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
        tick_id="TK-DRILL-001",
        feed_id="SYNTH-EQUITIES-L1",
        symbol="AAPL",
        bid=Decimal("190.00"),
        ask=Decimal("190.04"),
        last=Decimal("190.02"),
        volume=Decimal("1000"),
        sequence_no=1,
        event_time_utc=now - timedelta(milliseconds=20),
        available_time_utc=now - timedelta(milliseconds=10),
    )
    platform.process_tick(tick, now_utc=now)
    return platform, platform.broker, tick, now


def _intent(
    cid: str,
    idem: str,
    *,
    qty: str = "100",
    limit_px: str = "190.10",
    ref_px: str = "190.02",
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
        reference_price=Decimal(ref_px),
    )


def run_all_drills() -> dict[str, Any]:
    results: dict[str, Any] = {}

    # 1. Duplicate submit
    p, broker, tick, now = _base_platform()
    intent = _intent("ORD-DUP-001", "IDEM-DUP-001", qty="50")
    o1, _, rep1 = p.submit_manual_intent(intent, reference_tick=tick, now_utc=now)
    o2, _, rep2 = p.submit_manual_intent(intent, reference_tick=tick, now_utc=now)
    conflict_caught = False
    try:
        p.submit_manual_intent(
            _intent("ORD-DUP-002", "IDEM-DUP-001", qty="75"),
            reference_tick=tick,
            now_utc=now,
        )
    except DuplicateOrderConflictError:
        conflict_caught = True
    assert not rep1 and rep2 and o1.client_order_id == o2.client_order_id
    assert broker.submit_call_count == 1 and conflict_caught
    results["1_duplicate_submit"] = {
        "passed": True,
        "broker_submit_calls": broker.submit_call_count,
        "idempotent_replay_returned_same_order": o1.client_order_id == o2.client_order_id,
        "conflicting_payload_rejected": conflict_caught,
    }
    p.close()

    # 2. Unknown order outcome (retains reserved exposure until reconciled!)
    p, broker, tick, now = _base_platform()
    broker.inject_next_submit_timeout(land_on_broker=True, fill_on_broker=True)
    unk, _, _ = p.submit_manual_intent(
        _intent("ORD-UNK-001", "IDEM-UNK-001", qty="500"),
        reference_tick=tick,
        now_utc=now,
    )
    res_during_unknown = p.store.get_reservation(unk.client_order_id)
    assert unk.state == OrderState.UNKNOWN_OUTCOME
    assert res_during_unknown is not None and res_during_unknown.active
    assert res_during_unknown.reserved_notional == Decimal("95050.0000")

    # Because reserved exposure is retained, another 600-share order (exceeding 1000 max_symbol_position_qty
    # when combined with 500 reserved) MUST be rejected by risk!
    blocked, rchk, _ = p.submit_manual_intent(
        _intent("ORD-UNK-002", "IDEM-UNK-002", qty="500"),
        reference_tick=tick,
        now_utc=now,
    )
    # 500 reserved + 500 new = 1000 qty, notional 95050 + 95050 = 190100 <= 200000; try a third 100 qty to breach
    blocked3, rchk3, _ = p.submit_manual_intent(
        _intent("ORD-UNK-003", "IDEM-UNK-003", qty="100"),
        reference_tick=tick,
        now_utc=now,
    )
    assert blocked3.state == OrderState.RISK_REJECTED
    assert rchk3 is not None and any("R07_SYMBOL_POSITION_LIMIT" in b for b in rchk3.blocking_reasons)

    rec = p.reconcile(actor="DRILL_RECON")
    resolved = p.store.get_order(unk.client_order_id)
    res_after = p.store.get_reservation(unk.client_order_id)
    assert resolved is not None and resolved.state == OrderState.FILLED
    assert res_after is not None and not res_after.active
    assert rec.matched and rec.unknown_orders_resolved == 1
    results["2_unknown_order_outcome"] = {
        "passed": True,
        "state_after_timeout": OrderState.UNKNOWN_OUTCOME.value,
        "reservation_retained_notional": str(res_during_unknown.reserved_notional),
        "excess_order_blocked_by_retained_reserve": blocked3.state.value,
        "resolved_state_after_reconcile": resolved.state.value,
        "reconcile_matched": rec.matched,
    }
    p.close()

    # 3. Stale feed
    p, broker, _, now = _base_platform()
    stale_tick = MarketTick(
        tick_id="TK-STALE-001",
        feed_id="SYNTH-EQUITIES-L1",
        symbol="AAPL",
        bid=Decimal("190.00"),
        ask=Decimal("190.04"),
        last=Decimal("190.02"),
        volume=Decimal("500"),
        sequence_no=2,
        event_time_utc=now - timedelta(seconds=12),
        available_time_utc=now - timedelta(seconds=11),
    )
    stale_res = p.process_tick(stale_tick, now_utc=now)
    assert not stale_res.accepted and stale_res.rejection_code == "STALE_FEED"
    results["3_stale_feed"] = {
        "passed": True,
        "accepted": stale_res.accepted,
        "rejection_code": stale_res.rejection_code,
        "rejection_message": stale_res.rejection_message,
    }
    p.close()

    # 4. Broker disconnect
    p, broker, tick, now = _base_platform()
    broker.set_connected(False)
    disc_ord, _, _ = p.submit_manual_intent(
        _intent("ORD-DISC-001", "IDEM-DISC-001", qty="100"),
        reference_tick=tick,
        now_utc=now,
    )
    res_disc = p.store.get_reservation(disc_ord.client_order_id)
    assert disc_ord.state == OrderState.UNKNOWN_OUTCOME
    assert res_disc is not None and res_disc.active
    broker.set_connected(True)
    rec = p.reconcile(actor="DRILL_RECONNECT")
    after_ord = p.store.get_order(disc_ord.client_order_id)
    after_res = p.store.get_reservation(disc_ord.client_order_id)
    assert after_ord is not None and after_ord.state == OrderState.REJECTED
    assert after_res is not None and not after_res.active
    assert rec.matched
    results["4_broker_disconnect"] = {
        "passed": True,
        "state_during_outage": OrderState.UNKNOWN_OUTCOME.value,
        "reservation_retained_during_outage": True,
        "state_after_reconnect_reconcile": after_ord.state.value,
        "reservation_released_after_reconcile": not after_res.active,
        "reconcile_matched": rec.matched,
    }
    p.close()

    # 5. Cancel/fill race
    p, broker, tick, now = _base_platform()
    broker.set_resting_mode(True)
    resting, _, _ = p.submit_manual_intent(
        _intent("ORD-RACE-001", "IDEM-RACE-001", qty="100"),
        reference_tick=tick,
        now_utc=now,
    )
    assert resting.state == OrderState.ACKED
    broker.inject_next_cancel_fill_race(fill_ratio=Decimal("1.0"))
    raced = p.oms.cancel_order(resting.client_order_id, actor="DRILL", reason="Test cancel/fill race")
    rec = p.reconcile(actor="DRILL_RACE")
    assert raced.state == OrderState.FILLED
    assert raced.filled_quantity == Decimal("100")
    assert p.ledger.positions_map()["AAPL"] == Decimal("100")
    assert rec.matched and p.ledger.is_balanced()
    results["5_cancel_fill_race"] = {
        "passed": True,
        "final_state": raced.state.value,
        "filled_quantity": str(raced.filled_quantity),
        "ledger_balanced": p.ledger.is_balanced(),
        "reconcile_matched": rec.matched,
    }
    p.close()

    # 6. Restart recovery
    with tempfile.TemporaryDirectory() as tmpdir:
        db_file = str(Path(tmpdir) / "restart_drill.db")
        p1, broker1, tick1, now1 = _base_platform(db_path=db_file)
        broker1.inject_next_submit_timeout(land_on_broker=True, fill_on_broker=True)
        o_pre, _, _ = p1.submit_manual_intent(
            _intent("ORD-RST-001", "IDEM-RST-001", qty="80"),
            reference_tick=tick1,
            now_utc=now1,
        )
        p1.engage_kill_switch(
            scope=KillSwitchScope.SYMBOL,
            target="NVDA",
            reason="Pre-restart symbol halt",
            actor="drill-op",
            actor_role=Role.OPERATOR,
        )
        assert o_pre.state == OrderState.UNKNOWN_OUTCOME
        p1.close()

        # Restart fresh platform instance pointing to same SQLite DB & same broker
        cfg2 = PlatformConfig.from_json_file(ROOT / "config/platform.paper.json").model_copy(
            update={"db_path": db_file}
        )
        p2 = TradingPlatform(cfg2, broker=broker1)
        recovery = p2.recover_after_restart()
        o_post = p2.store.get_order("ORD-RST-001")
        switches = p2.store.list_kill_switches(engaged_only=True)
        assert o_post is not None and o_post.state == OrderState.FILLED
        assert len(switches) == 1 and switches[0].target == "NVDA"
        assert recovery["audit_chain_valid"] and recovery["reconciled"]
        results["6_restart"] = {
            "passed": True,
            "recovery_summary": recovery,
            "recovered_order_state": o_post.state.value,
            "persisted_kill_switches": [f"{s.scope.value}:{s.target}" for s in switches],
        }
        p2.close()

    # 7. Limit breach
    p, broker, tick, now = _base_platform()
    fat_finger, ff_chk, _ = p.submit_manual_intent(
        _intent("ORD-LIM-001", "IDEM-LIM-001", qty="50", limit_px="210.00"),
        reference_tick=tick,
        now_utc=now,
    )
    oversize, os_chk, _ = p.submit_manual_intent(
        _intent("ORD-LIM-002", "IDEM-LIM-002", qty="600", limit_px="190.10"),
        reference_tick=tick,
        now_utc=now,
    )
    assert fat_finger.state == OrderState.RISK_REJECTED
    assert oversize.state == OrderState.RISK_REJECTED
    assert broker.submit_call_count == 0
    results["7_limit_breach"] = {
        "passed": True,
        "fat_finger_blocked": ff_chk.blocking_reasons if ff_chk else [],
        "oversize_blocked": os_chk.blocking_reasons if os_chk else [],
        "broker_submit_calls": broker.submit_call_count,
    }
    p.close()

    # 8. Clock skew
    p, broker, _, now = _base_platform()
    skew_tick = MarketTick(
        tick_id="TK-SKEW-001",
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
    skew_res = p.process_tick(skew_tick, now_utc=now)
    assert not skew_res.accepted and skew_res.rejection_code == "CLOCK_SKEW_DETECTED"
    results["8_clock_skew"] = {
        "passed": True,
        "accepted": skew_res.accepted,
        "rejection_code": skew_res.rejection_code,
        "rejection_message": skew_res.rejection_message,
    }
    p.close()

    evidence = {
        "suite": "plan_section_5_mandatory_fault_drills",
        "executed_at_utc": datetime.now(UTC).isoformat(),
        "total_scenarios": len(results),
        "all_passed": all(r["passed"] for r in results.values()),
        "scenarios": results,
    }
    out_path = ROOT / "reports/evidence/fault_drills.json"
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(evidence, indent=2, default=str), encoding="utf-8")
    return evidence


if __name__ == "__main__":
    report = run_all_drills()
    print(json.dumps(report, indent=2, default=str))
    sys.exit(0 if report["all_passed"] else 1)
