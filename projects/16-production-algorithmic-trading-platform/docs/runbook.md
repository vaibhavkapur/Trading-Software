# Operator Runbook & Incident Response Procedures

## 1. Safety Posture & Default State

- **Default Execution Mode**: `PAPER` (`POL-PAPER-ONLY-DEFAULT-001`). Live trading is strictly disabled by code and configuration.
- **Operator Console**: Launch via `.venv/bin/patp serve` (binds `127.0.0.1:8080` by default).
- **Primary Safe Fallback**: Engage the **Global Kill Switch** (`POST /api/kill-switch/engage` or `patp kill-engage`) to immediately halt all new order dispatch and cancel resting open orders.

## 2. Incident Playbooks (All 8 Mandatory Failure Modes)

| # | Incident / Fault | Automated System Behavior | Operator Verification & Recovery |
|---|---|---|---|
| 1 | **Duplicate Submit** | OMS checks `idempotency_key` + SHA-256 `payload_hash`. Identical retries return the original `OrderRecord` without calling broker. Conflicting payload raises `DuplicateOrderConflictError` and logs audit event. | Inspect `/api/audit` for `DUPLICATE_ORDER_CONFLICT`; confirm upstream strategy scheduler did not corrupt client IDs. |
| 2 | **Unknown Order Outcome** | On broker timeout, OMS transitions order to `UNKNOWN_OUTCOME` and **retains active `RiskReservation`**, preventing new orders from breaching limits. | Wait for broker stability, then run `POST /api/reconcile`. Verify `unknown_orders_resolved >= 1`, `matched == true`, and reservation released/settled. |
| 3 | **Stale Feed** | `MarketDataGuard` rejects ticks with age `> max_staleness_ms` (`5000ms`) with `STALE_FEED`, increments `market_ticks_stale`, and prevents strategy evaluation. | Check feed upstream health and `feed_staleness_ms.*` gauge in `/api/metrics`. Do not override staleness thresholds without Risk Officer approval. |
| 4 | **Broker Disconnect** | Submit/cancel raises `BrokerDisconnectedError`; inflight orders move to `UNKNOWN_OUTCOME` with retained reservations. `/api/health` reports `broker_connected: false`. | Engage global kill switch if outage persists `> 30s`. Once broker reconnects, run `POST /api/reconcile` before releasing kill switch. |
| 5 | **Cancel/Fill Race** | If a fill arrives while order is `CANCEL_PENDING`, OMS applies fill first: full fill transitions to terminal `FILLED`; partial fill + cancel transitions to `CANCELED` and releases only unfilled reserve. | Confirm `/api/reconcile` matches broker cash & position to `$0.0000` and `/api/ledger` reports `balanced: true`. |
| 6 | **Process Crash / Restart** | SQLite WAL preserves all orders, idempotency keys, active reservations, kill switches, double-entry ledger, and hash-chained audit events. | On startup, invoke `recover_after_restart()` (verifies audit chain, verifies ledger balance, and reconciles uncertain orders before resuming). |
| 7 | **Risk Limit Breach** | `RiskEngine` rejects breaching orders (`R01`–`R10`) as `RISK_REJECTED` before broker dispatch and writes `ORDER_RISK_REJECTED` audit record. | Review `blocking_reasons` on rejected orders. Never raise risk limits during an active incident without formal Risk Officer sign-off. |
| 8 | **Clock Skew** | `MarketDataGuard` rejects forward wall-clock drift `> max_clock_skew_ms` (`2000ms`) or inverted `event_time > available_time` with `CLOCK_SKEW_DETECTED`. | Verify NTP/PTP host synchronization (`chronyc`/`sntp`) and upstream feed timestamping before resuming ingestion. |

## 3. Dual-Control Kill Switch Release

When a kill switch is engaged by a critical reconciliation break (or manually with `requires_dual_release=true`), a single operator cannot clear it:
1. Confirm root cause is resolved and `POST /api/reconcile` returns `matched: true`.
2. Obtain secondary approval from a distinct actor holding `RISK_OFFICER`, `COMPLIANCE`, or `CTO` role.
3. Submit `POST /api/kill-switch/release` with `secondary_approver` and `secondary_role`.
4. Verify `KILL_SWITCH_RELEASED` is recorded in `/api/audit` and hash chain remains `valid: true`.
