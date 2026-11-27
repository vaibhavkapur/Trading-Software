#!/usr/bin/env python3
"""End-to-end paper-first vertical slice + shadow mode + reconciliation demonstration."""

from __future__ import annotations

import json
import sys
from datetime import UTC, datetime, timedelta
from decimal import Decimal
from pathlib import Path

from trading_platform.domain.config import PlatformConfig
from trading_platform.domain.events import (
    FeedEntitlement,
    KillSwitchScope,
    MarketTick,
    Role,
)
from trading_platform.platform import TradingPlatform

ROOT = Path(__file__).resolve().parent.parent


def load_entitlements() -> list[FeedEntitlement]:
    manifest = json.loads((ROOT / "config/fixtures_manifest.json").read_text(encoding="utf-8"))
    return [FeedEntitlement.model_validate(e) for e in manifest["entitlements"]]


def load_ticks(fixture_path: Path, base_time: datetime) -> list[tuple[MarketTick, datetime]]:
    raw = json.loads(fixture_path.read_text(encoding="utf-8"))
    feed_id = raw["feed_id"]
    ccy = raw["currency"]
    out: list[tuple[MarketTick, datetime]] = []
    for item in raw["ticks"]:
        ts = base_time + timedelta(milliseconds=int(item["offset_ms"]))
        tick = MarketTick(
            tick_id=item["tick_id"],
            feed_id=feed_id,
            symbol=item["symbol"],
            currency=ccy,
            bid=Decimal(item["bid"]),
            ask=Decimal(item["ask"]),
            last=Decimal(item["last"]),
            volume=Decimal(item["volume"]),
            sequence_no=int(item["sequence_no"]),
            event_time_utc=ts,
            available_time_utc=ts + timedelta(milliseconds=5),
        )
        out.append((tick, ts + timedelta(milliseconds=15)))
    return out


def run_demo() -> dict[str, object]:
    base_time = datetime(2026, 11, 27, 14, 30, 0, tzinfo=UTC)
    entitlements = load_entitlements()

    # 1. Paper-mode vertical slice
    paper_cfg = PlatformConfig.from_json_file(ROOT / "config/platform.paper.json").model_copy(
        update={"db_path": ":memory:"}
    )
    paper = TradingPlatform(paper_cfg)
    for ent in entitlements:
        paper.register_entitlement(ent)

    paper_ticks = load_ticks(ROOT / "data/fixtures/synthetic_paper_feed.json", base_time)
    for tick, sim_now in paper_ticks:
        res = paper.process_tick(tick, now_utc=sim_now)
        assert res.accepted, f"Expected fixture tick {tick.tick_id} to be accepted"

    recon_report = paper.reconcile(actor="DEMO_OPERATOR")
    assert recon_report.matched, f"Expected clean paper reconciliation: {recon_report.breaks}"

    # Verify kill switch blocks trading and can be released cleanly
    ks_state, _ = paper.engage_kill_switch(
        scope=KillSwitchScope.GLOBAL,
        target="ALL",
        reason="Demo kill switch verification",
        actor="demo-risk-officer",
        actor_role=Role.RISK_OFFICER,
    )
    assert ks_state.engaged
    paper.release_kill_switch(
        scope=KillSwitchScope.GLOBAL,
        target="ALL",
        reason="Demo kill switch cleared",
        actor="demo-risk-officer",
        actor_role=Role.RISK_OFFICER,
    )

    paper_status = paper.status_summary()
    paper.close()

    # 2. Entitled Shadow Mode verification (zero broker dispatch)
    shadow_cfg = PlatformConfig.from_json_file(ROOT / "config/platform.shadow.json").model_copy(
        update={"db_path": ":memory:"}
    )
    shadow = TradingPlatform(shadow_cfg)
    for ent in entitlements:
        shadow.register_entitlement(ent)

    shadow_ticks = load_ticks(ROOT / "data/fixtures/entitled_shadow_feed.json", base_time)
    for tick, sim_now in shadow_ticks:
        res = shadow.process_tick(tick, now_utc=sim_now)
        assert res.accepted

    shadow_decisions = shadow.store.list_shadow_decisions()
    shadow_orders = shadow.store.list_orders()
    assert len(shadow_decisions) >= 1
    assert len(shadow_orders) == 0, "Shadow mode must NEVER dispatch orders to OMS/broker"
    assert all(not d.dispatched_to_broker for d in shadow_decisions)
    shadow_summary = {
        "execution_mode": shadow.config.execution_policy.execution_mode.value,
        "ticks_processed": len(shadow_ticks),
        "shadow_decisions_recorded": len(shadow_decisions),
        "broker_orders_dispatched": len(shadow_orders),
        "decisions": [d.model_dump(mode="json") for d in shadow_decisions],
    }
    shadow.close()

    evidence = {
        "demo": "vertical_slice_and_shadow_mode",
        "paper_mode": {
            "ticks_processed": len(paper_ticks),
            "orders_summary": paper_status["orders"],
            "ledger": paper_status["ledger"],
            "reconciliation_matched": recon_report.matched,
            "reconciliation_cash_delta": str(recon_report.cash_delta),
            "audit_chain": paper_status["audit_chain"],
            "live_gate": paper_status["live_gate"],
            "telemetry_counters": paper_status["telemetry"]["counters"],
            "telemetry_histograms": paper_status["telemetry"]["histograms"],
        },
        "shadow_mode": shadow_summary,
    }

    evidence_path = ROOT / "reports/evidence/demo_run.json"
    evidence_path.parent.mkdir(parents=True, exist_ok=True)
    evidence_path.write_text(json.dumps(evidence, indent=2, default=str), encoding="utf-8")
    return evidence


if __name__ == "__main__":
    result = run_demo()
    print(json.dumps(result, indent=2, default=str))
    sys.exit(0)
