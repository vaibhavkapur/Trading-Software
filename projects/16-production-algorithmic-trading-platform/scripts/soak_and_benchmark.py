#!/usr/bin/env python3
"""Controlled paper soak test and baseline latency/throughput measurement (M4 evidence)."""

from __future__ import annotations

import json
import math
import platform as py_platform
import random
import sys
import time
from datetime import UTC, datetime, timedelta
from decimal import Decimal
from pathlib import Path
from typing import Any

from trading_platform.domain.config import PlatformConfig
from trading_platform.domain.events import FeedEntitlement, MarketTick
from trading_platform.platform import TradingPlatform

ROOT = Path(__file__).resolve().parent.parent


def run_soak_and_benchmark(num_ticks: int = 2000, seed: int = 2026) -> dict[str, Any]:
    rng = random.Random(seed)
    cfg = PlatformConfig.from_json_file(ROOT / "config/platform.paper.json").model_copy(
        update={
            "db_path": ":memory:",
            "risk_limits": PlatformConfig.from_json_file(
                ROOT / "config/platform.paper.json"
            ).risk_limits.model_copy(
                update={
                    "max_orders_per_minute_per_strategy": 10000,
                    "max_symbol_position_qty": Decimal("50000"),
                    "max_symbol_notional": Decimal("25000000"),
                    "max_gross_notional": Decimal("50000000"),
                }
            ),
        }
    )
    app = TradingPlatform(cfg)
    app.register_entitlement(
        FeedEntitlement(
            feed_id="SYNTH-SOAK-L1",
            provider="SAAT_SOAK_SIM",
            license_type="SYNTHETIC_INTERNAL_PERMITTED",
            symbols=["AAPL", "MSFT", "NVDA", "SPY"],
            provenance_uri="scripts/soak_and_benchmark.py",
            valid_until_utc=datetime(2030, 1, 1, tzinfo=UTC),
        )
    )

    symbols = ["AAPL", "MSFT", "NVDA", "SPY"]
    prices = {"AAPL": 190.0, "MSFT": 420.0, "NVDA": 130.0, "SPY": 580.0}
    seqs = {s: 0 for s in symbols}
    base_time = datetime(2026, 11, 27, 14, 0, 0, tzinfo=UTC)

    t_start = time.perf_counter()
    for i in range(num_ticks):
        sym = symbols[i % len(symbols)]
        seqs[sym] += 1
        # Deterministic oscillatory + random walk to trigger realistic crossover and mean-reversion fills
        wave = 0.65 * math.sin(i / 18.0) + rng.gauss(0.0, 0.35)
        prices[sym] = max(10.0, prices[sym] + wave)
        mid = Decimal(f"{prices[sym]:.2f}")
        bid = mid - Decimal("0.02")
        ask = mid + Decimal("0.02")
        ts = base_time + timedelta(milliseconds=i * 250)
        tick = MarketTick(
            tick_id=f"SOAK-{i:06d}",
            feed_id="SYNTH-SOAK-L1",
            symbol=sym,
            bid=bid,
            ask=ask,
            last=mid,
            volume=Decimal("1000"),
            sequence_no=seqs[sym],
            event_time_utc=ts,
            available_time_utc=ts + timedelta(milliseconds=2),
        )
        res = app.process_tick(tick, now_utc=ts + timedelta(milliseconds=5))
        assert res.accepted, f"Soak tick failed: {res.rejection_message}"

    elapsed_sec = time.perf_counter() - t_start
    recon = app.reconcile(actor="SOAK_RUNNER")
    audit_ok, audit_msg = app.store.verify_audit_chain()
    status = app.status_summary()
    app.close()

    assert recon.matched, f"Soak reconciliation failed: {recon.breaks}"
    assert status["ledger"]["balanced"], "Double-entry ledger unbalanced after soak"
    assert audit_ok, f"Audit hash chain failed after soak: {audit_msg}"
    assert status["reservations"]["active_count"] == 0

    report: dict[str, Any] = {
        "benchmark": "controlled_paper_soak_baseline",
        "disclaimer": (
            "Measured on local in-process SQLite paper sandbox only. "
            "Do NOT extrapolate these numbers to live exchange latency, production availability, "
            "or live alpha performance."
        ),
        "environment": {
            "python_version": py_platform.python_version(),
            "platform": py_platform.platform(),
            "machine": py_platform.machine(),
            "storage": "sqlite_in_memory",
            "seed": seed,
        },
        "sample": {
            "ticks_processed": num_ticks,
            "elapsed_seconds": round(elapsed_sec, 4),
            "throughput_ticks_per_sec": round(num_ticks / elapsed_sec, 2),
        },
        "orders": status["orders"],
        "ledger_balanced": status["ledger"]["balanced"],
        "reconciliation_matched": recon.matched,
        "reconciliation_cash_delta": str(recon.cash_delta),
        "active_reservations_end": status["reservations"]["active_count"],
        "audit_chain_valid": audit_ok,
        "latency_histograms_ms": status["telemetry"]["histograms"],
    }

    out_path = ROOT / "reports/evidence/soak_and_benchmark.json"
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(report, indent=2, default=str), encoding="utf-8")
    return report


if __name__ == "__main__":
    data = run_soak_and_benchmark()
    print(json.dumps(data, indent=2, default=str))
    sys.exit(0)
