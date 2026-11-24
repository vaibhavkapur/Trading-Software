# Operational Runbook — Options Trading Infrastructure (PAPER / SIMULATED)

## Service Startup

```bash
make setup
make serve
# Opens API and Web UI on http://127.0.0.1:8011
```

Environment variables (all optional; defaults run entirely locally with simulated data):

- `OPTIONS_INFRA_DB_PATH`: SQLite path for paper orders & positions (default `:memory:`; Docker uses `/app/data/paper_orders.sqlite3`).
- `OPTIONS_INFRA_FIXTURE_PATH`: Path to deterministic JSON option chain fixture (defaults to built-in simulated `XYZ` + `ABC` universe).
- `OPTIONS_INFRA_MAX_QUOTE_AGE_SECONDS`: Maximum allowed quote age in seconds before `STALE_QUOTE` blocks orders/strict risk (default `300`).
- `OPTIONS_INFRA_MAX_CLOCK_SKEW_SECONDS`: Maximum allowed `|asof - received_at|` skew in seconds before `CLOCK_SKEW_DETECTED` blocks orders (default `30`).

## Health & Observability

- **Health check:** `GET /health`
  - Returns `status: "ok"` when simulated feed is connected, `"degraded"` when disconnected.
  - Always confirms `execution_mode: "PAPER_SIMULATED"`.
- **Prometheus metrics:** `GET /metrics` (or JSON summary at `GET /v1/metrics`):
  - `options_pricing_reference_error_abs` — verify `< 1e-10` for BSM parity.
  - `options_iv_solver_convergence_rate` — alert if `< 0.98` on active liquid chains.
  - `options_risk_recalc_latency_seconds` — monitor p99 recalculation latency.
  - `options_portfolio_net_delta`, `options_portfolio_net_gamma`, `options_portfolio_net_vega_per_1pct`, `options_portfolio_net_theta_per_day`.
  - `options_scenario_worst_case_pnl`, `options_scenario_best_case_pnl`.

## Failure Modes & Triage

| Symptom | HTTP / Order Status | Root Cause | Mitigation / Recovery |
| --- | --- | --- | --- |
| `/health` reports `degraded`, chain returns `503` | `503 Service Unavailable`; orders `REJECTED` (`MARKET_DATA_UNAVAILABLE`) | Market data feed disconnected | Restore feed connectivity; verify `latest_quote_asof` advances before retrying orders. |
| Risk returns `409`; orders `REJECTED` (`data-freshness guard`) | `409 Conflict` / `REJECTED` | Quotes older than `OPTIONS_INFRA_MAX_QUOTE_AGE_SECONDS` | Inspect quote feed lag; ingest fresh quotes; never disable freshness guards in live/paper automation without review. |
| Orders `REJECTED` (`clock-skew guard`) | `REJECTED` | Venue `asof` diverges from local `received_at` beyond threshold | Check NTP/host clock sync and upstream feed timestamps. |
| Orders `REJECTED` (`BROKER_CONSTRAINT_FAILED`) | `REJECTED` | Unsupported multi-leg structure, uncovered short, invalid tick size, or expired contract | Inspect `rejection_reason`; ensure legs share underlying/expiry and satisfy defined-risk rules. |
| Order submission returns `503` (`write failure`) | `503 Service Unavailable` | SQLite storage write error or disk full | Inspect DB volume permissions/space; retry using the same `idempotency_key` once healthy. |
| Duplicate client retry after timeout | `200 OK` with `deduplicated: true` | Same `idempotency_key` replayed | Expected safe behavior — returns original order without double-filling positions. |
