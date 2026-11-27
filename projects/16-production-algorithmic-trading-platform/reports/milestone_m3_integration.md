# Milestone M3 — Integration and Validation Report

## Deliverables Completed
1. **Entitled Shadow Mode**: `config/platform.shadow.json` + `data/fixtures/entitled_shadow_feed.json` runs strategies and risk evaluation over entitled feeds, persisting `ShadowDecisionRecord` entries with `dispatched_to_broker = false` and **0** broker orders dispatched.
2. **Broker Reconciliation**: `BrokerReconciler` reconciles OMS orders, reserved exposure, ledger cash, and positions against live broker snapshots or external fixtures (`data/fixtures/broker_reconciliation_fixture.json`), resolving `UNKNOWN_OUTCOME` orders and triggering a dual-release global kill switch on critical breaks.
3. **Kill Switch & Operator Console**: Scoped kill switches (`GLOBAL`, `STRATEGY`, `SYMBOL`) with Maker-Checker dual release, RBAC-protected REST API (`src/trading_platform/api/server.py`), `patp` CLI, and zero-CDN web console (`src/trading_platform/web/index.html`).
4. **Observability**: `TelemetryRecorder` captures counters, gauges, trace spans, and latency histograms (p50/p95/p99) across tick ingestion, risk evaluation, OMS submission, and reconciliation.

## All 8 Mandatory Fault Drills ([fault_drills.json](evidence/fault_drills.json))

| # | Scenario | Status | Key Verified Outcome |
|---|---|---|---|
| 1 | Duplicate submit | PASS | `broker_submit_calls = 1`; identical replay returns original order; conflicting payload raises `DuplicateOrderConflictError`. |
| 2 | Unknown order outcome | PASS | Timeout transitions order to `UNKNOWN_OUTCOME`, retains `\$95,050.0000` reserved exposure, blocks excess order via `R07_SYMBOL_POSITION_LIMIT`, and resolves to `FILLED` on reconcile. |
| 3 | Stale feed | PASS | 12,000ms stale tick rejected with `STALE_FEED` (max 5,000ms); 0 orders generated. |
| 4 | Broker disconnect | PASS | Outage marks order `UNKNOWN_OUTCOME` with retained reserve; reconnect + reconcile marks `REJECTED` and releases reserve (`matched = true`). |
| 5 | Cancel/fill race | PASS | Full fill wins race (`FILLED`, 100 qty); partial fill race cancels remainder and preserves filled leg (`matched = true`, `ledger_balanced = true`). |
| 6 | Restart | PASS | Fresh instance reloads SQLite WAL, preserves kill switch (`SYMBOL:NVDA`) and idempotency keys, validates audit chain, and reconciles `UNKNOWN_OUTCOME -> FILLED`. |
| 7 | Limit breach | PASS | Fat-finger (`1051.47 bps > 250 bps`), qty (`600 > 500`), notional (`114,060 > 100,000`), and disallowed symbol rejected pre-dispatch (`broker_submit_calls = 0`). |
| 8 | Clock skew | PASS | +10,000ms forward drift and inverted `event_time > available_time` rejected with `CLOCK_SKEW_DETECTED`. |
