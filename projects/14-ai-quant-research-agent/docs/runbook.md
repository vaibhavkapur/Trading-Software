# Operations Runbook — AI Quant Research Agent

## Startup & Health Verification

```bash
make setup
make serve
curl -s http://127.0.0.1:8014/health | python3 -m json.tool
```

Confirm `"mode": "PAPER_SIMULATED"` and `"paper_simulated_only": true`.

## Common Operational Conditions & Responses

### 1. Spike in `quant_agent_rejected_experiments_total`
- Query `GET /v1/metrics` to inspect `rejected_invalid_or_leaky_experiments.by_reason`.
- `SPLIT_OVERLAP_LEAKAGE` / `INSUFFICIENT_PURGE_GAP` / `NEGATIVE_FEATURE_SHIFT`: Expected defense against leaky configurations; review planner templates or user-submitted configs.
- `DATA_STALE_BARS` / `CLOCK_SKEW_VIOLATION` / `MISSING_DATA_GAPS`: Indicates upstream market-data feed degradation. Keep experiments blocked until feed integrity is restored and checksums re-verified.
- `CHECKSUM_MISMATCH`: Treat as critical data tampering or storage corruption; do not bypass validation.

### 2. Registry Storage Disconnect or Read-Only Mode (HTTP 503)
- Symptom: Endpoints return `503 Service Unavailable` with `"Registry storage is disconnected"` or `"read-only mode"`.
- Impact: Fail-closed behavior prevents partial or unrecorded backtest runs from executing without an audit trail.
- Recovery: Verify disk space and SQLite WAL lock health at `QUANT_AGENT_DATA_DIR/registry.db`, restore write permissions, and confirm `GET /v1/research/memory` returns `200 OK`.

### 3. Reproducibility Verification Failure
- Run `POST /v1/research/runs/{run_id}/reproduce`.
- If `reproducible` is `false`, compare `config_hash`, `dataset_checksum`, `code_revision`, and `seed` in `GET /v1/research/runs/{run_id}/artifacts` (`manifest`).
- Never promote a report whose reproducibility check fails.

### 4. Peer-Review Authorization Errors (HTTP 401 / 403)
- When `QUANT_AGENT_REQUIRE_AUTH=true`, researcher mutations require `X-API-Token: $QUANT_AGENT_RESEARCHER_TOKEN` and peer-review sign-off requires `X-API-Token: $QUANT_AGENT_REVIEWER_TOKEN`.
- Tokens must be injected via environment variables or a secret manager, never committed to source control.
