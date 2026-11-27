# M4 — Hardening, Security Review, and Reconciliation Drill Report

## Scope Completed
- Executed the **Shared Broker Contract Suite** (`TestSharedBrokerContractSuite`) across all 3 adapters (`mock`, `sandbox-alpha`, `sandbox-beta`) — **100% PASS** with Go race detector (`-race`).
- Verified all **7 required failure scenarios** (`tests/scenarios/failure_scenarios_test.go` + reproducible `./scripts/reproduce.sh` drill):
  1. **Rate limit:** Upstream 429 `Retry-After` propagated to local cooldown (0 broker dispatches during cooldown); clean recovery after window.
  2. **Expired credential:** Rejected with `AUTH_EXPIRED`, zero secrets in logs (`secret_leak_detected: false`), recovered cleanly after rotation.
  3. **Symbol mismatch:** Rejected pre-dispatch (`SYMBOL_MISMATCH`) with `zero_dispatches: true`.
  4. **Duplicate webhook:** Duplicate `event_id` and duplicate `broker_fill_id` ignored (`duplicate_applied: false`); exact filled qty & VWAP preserved.
  5. **Out-of-order fill:** Sequence #2 arriving before #1 and stale `ACCEPTED` arriving after `FILLED` handled monotonically (`avg_fill_price: 190.24`, sorted fills `[1, 2]`).
  6. **Unknown submit result:** Transitioned to `SUBMIT_UNKNOWN`, **never blindly retried** (`no_blind_retry: true`), reconciled cleanly to broker truth (`FILLED` when accepted, `REJECTED` when absent).
  7. **Cancel timeout:** Transitioned to `CANCEL_PENDING`, reconciled to `CANCELLED` / `FILLED` when terminal, and tripped fail-closed halt when stuck non-terminal.
- Executed fail-closed **Reconciliation Drift Drill** (phantom broker order tripped `triggered_fail_safe: true`, blocked subsequent orders with `GATEWAY_HALTED`, resumed cleanly via operator action).
- Completed [`docs/security_review.md`](../docs/security_review.md), [`docs/runbook.md`](../docs/runbook.md), [`docs/unsupported_features.md`](../docs/unsupported_features.md), and [`docs/adr/ADR-003-fail-closed-gateway-and-sandbox-policy.md`](../docs/adr/ADR-003-fail-closed-gateway-and-sandbox-policy.md).

## Baseline Measurements (Local Prototype Only — Not Live Production Claims)
- **Environment:** `darwin/arm64`, Apple M5, `go1.27.1`, in-memory `MemoryStore` + `mock` adapter (no network I/O).
- **Benchmark (`BenchmarkGatewaySubmitOrder`, 3 runs, ~41k–43k ops/run):**
  - `28,591 ns/op` / `28,035 ns/op` / `27,969 ns/op` (~28.2 µs/op mean, ~35.5k submits/sec single-threaded)
  - `9,267–9,424 B/op`, `113 allocs/op`
- Raw benchmark evidence: [`reports/evidence/benchmark_output.txt`](evidence/benchmark_output.txt).
- Raw drill evidence: [`reports/evidence/drill_output.json`](evidence/drill_output.json).
