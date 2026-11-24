# Operational Runbook — Cross-Exchange Arbitrage Monitor

> **Safety Boundary:** This service is **read-only and paper/simulated by default**. It never submits orders or moves funds.

## Quick Checks

```bash
# Check service liveness and paper mode
curl -s http://localhost:8080/healthz

# Inspect venue connectivity, staleness, and sequence integrity
curl -s http://localhost:8080/v1/venues/health

# Query live opportunities above 5 bps net edge
curl -s "http://localhost:8080/v1/opportunities?min_net_bps=5"

# Inspect Prometheus metrics
curl -s http://localhost:8080/metrics
```

## Incident Playbooks

### 1. Venue Marked `disconnected` or `resyncing`
- **Symptoms:** `arb_venue_disconnects_total` or `arb_venue_sequence_gaps_total` increases; `/v1/venues/health` reports `status: "disconnected"` or `"resyncing"`; `arb_suppressions_total{reason="venue_disconnected"}` increments.
- **Expected Behavior:** The monitor automatically suppresses all opportunities involving the affected venue (fail-closed) until a reconnect occurs **and** a full book snapshot (`is_snapshot: true`) is ingested.
- **Action:**
  1. Check upstream feed connectivity and rate-limit headers.
  2. Verify backoff recovery triggers snapshot resync.
  3. Confirm venue returns to `status: "healthy"` in `/v1/venues/health`.

### 2. High `stale_quote` or `clock_skew` Suppressions
- **Symptoms:** `arb_suppressions_total{reason="stale_quote"}` or `reason="clock_skew"` spikes; `arb_venue_quote_age_ms` exceeds `max_quote_age_ms`.
- **Action:**
  1. Verify host NTP / chrony synchronization.
  2. Compare `last_asof` vs `last_received_at` (`clock_skew_ms`) in `/v1/venues/health` to isolate exchange-side lag vs local network delay.
  3. Do **not** blindly raise `max_quote_age_ms` to force alerts; stale quotes create adverse-selection phantom signals.

### 3. Alert Store Write Failures (`arb_store_write_errors_total > 0`)
- **Symptoms:** Alert persistence returns an error; `arb_store_write_errors_total` increments.
- **Expected Behavior:** Cooldown is **not** armed on failed writes, preventing silent alert loss while surfacing write failures explicitly in logs and metrics.
- **Action:** Inspect disk space, permissions, or mounted volume health on the archive path.

### 4. Reproducing an Alert via Deterministic Replay
```bash
go run ./src/cmd/arb-replay \
  -feed examples/fixtures/recorded_books.jsonl \
  -instruments examples/config/instruments.json \
  -fees examples/config/fees.json \
  -balances examples/config/balances.json \
  -alert-config examples/config/alert_config.json \
  -verify-determinism=true
```
Compare the emitted `digest_sha256` against the archived replay baseline. Any change in fee schedule, alert config, or rule version will visibly change the alert evidence metadata (`config_version`, `rule_version`) and digest.
