# Operator Runbook — Multi-Broker Trading Gateway

## 1. Normal Operations & Health Monitoring

- **Health Check:** `GET /health`
  - `200 OK` (`status: "ok"`): Gateway open in `paper`/`sandbox` mode; inspect `broker_health` for per-venue auth/connectivity status.
  - `503 Service Unavailable` (`status: "halted"`): Fail-closed circuit breaker is active. All new `POST /v1/orders` requests are rejected with `GATEWAY_HALTED`.
- **Telemetry:** `GET /v1/metrics`
  - Watch `unknown_outcomes`, `cancel_timeouts`, `auth_failures`, `rate_limit_rejections`, and `fail_closed_trips`.

## 2. Incident Response Procedures

### A. Unknown Submit Outcome (`SUBMIT_UNKNOWN` / `UNKNOWN_OUTCOME`)
1. **Do NOT manually re-submit** the same order or retry with a new `client_order_id` before reconciling.
2. Trigger reconciliation: `POST /v1/reconcile?broker_id=<broker_id>`.
3. Inspect the returned `ReconciliationReport`:
   - If the order existed on the broker, local state is now synchronized (`ACCEPTED`/`FILLED`) with broker fills imported.
   - If the order never reached the broker, local state is marked `REJECTED` (`reconciled: true`), and the upstream OMS may safely decide whether to issue a new `client_order_id`.

### B. Cancel Timeout (`CANCEL_PENDING` / `CANCEL_TIMEOUT`)
1. Trigger reconciliation: `POST /v1/reconcile?broker_id=<broker_id>`.
2. If the broker resolved the order to `CANCELLED` or `FILLED`, reconciliation syncs local state cleanly.
3. If the broker still reports a stuck non-terminal state, the reconciler trips fail-closed halt (`triggered_fail_safe: true`). Contact broker support / check venue portal before resuming.

### C. Expired Credential (`AUTH_EXPIRED`)
1. Rotate the credential in the Secret Manager (never write credentials into `config/` or shell history).
2. Verify `GET /health` shows `"ok"` for the affected broker.
3. Re-run `POST /v1/reconcile?broker_id=<broker_id>` to confirm zero missed updates during the auth outage.

### D. Fail-Closed Halt Recovery (Phantom Order / Unresolved Drift)
1. Inspect `GET /v1/reconcile` to read the latest `ReconciliationReport` findings (`PHANTOM_BROKER_ORDER`, `FILL_QTY_MISMATCH`, or `MISSING_BROKER_ORDER`).
2. Audit broker venue state and resolve external/manual positions out-of-band.
3. Re-run `POST /v1/reconcile` to verify drift is understood/resolved.
4. Explicitly lift the halt with operator identity and audit note:
   ```bash
   curl -X POST http://127.0.0.1:8080/v1/operator/resume \
     -H "Content-Type: application/json" \
     -d '{"operator_id":"oncall-engineer","note":"verified broker position & cleared drift"}'
   ```

### E. Crash Recovery & Event Replay
1. Restart gateway pointing to the same durable journal (`data/runtime/gateway_journal.jsonl` or PostgreSQL database).
2. Startup automatically replays all orders, fills, webhook dedup keys, and replay cursors.
3. For replay-capable brokers (e.g., `sandbox-beta`), run event replay + `POST /v1/reconcile` before accepting new traffic.
