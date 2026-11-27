# ADR-002: Deterministic Pre-Trade Risk and Unknown-Outcome Exposure Retention

- **Status**: Accepted
- **Milestone**: M2 / M3 — Core Implementation & Fault Tolerance

## Context

When a network timeout or broker disconnect occurs after an OMS dispatches an order (`SUBMITTING`), the OMS cannot know whether the broker accepted, filled, or dropped the order. Releasing the risk reservation prematurely allows subsequent strategy signals to breach symbol or portfolio limits if the timed-out order actually filled on the venue.

## Decision

1. **Atomic Pre-Dispatch Reservation**: `RiskEngine.reserve(intent)` persists an active `RiskReservation` in durable storage *before* `BrokerAdapter.submit_order()` is called.
2. **Fail-Closed Retention on Ambiguity**: Any `BrokerTimeoutError` or `BrokerDisconnectedError` transitions the order to `UNKNOWN_OUTCOME` and calls `RiskEngine.retain_reservation_unknown(...)`, keeping `active=True` with an explicit `retained_reason`.
3. **Worst-Case Exposure Counting**: Rules `R07_SYMBOL_POSITION_LIMIT` and `R08_GROSS_EXPOSURE_LIMIT` always include `settled positions + all active reservations (open + UNKNOWN_OUTCOME) + incoming order`.
4. **Reconciliation-Only Release**: An `UNKNOWN_OUTCOME` reservation is only converted to a settled position or released once `BrokerReconciler` confirms authoritative broker state (`FILLED`, `CANCELED`, `REJECTED`, or confirmed absent).

## Consequences

- Prevents silent double-allocation or limit overrun during broker timeouts and network partitions.
- Slightly conservative buying power during outages (intentional fail-safe bias).
- Verified directly by `test_scenario_2_unknown_order_outcome_retains_reserved_exposure` and `scripts/fault_drills.py`.
