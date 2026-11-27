# API and Canonical Schema Documentation

## 1. REST Endpoints

| Method | Path | Description | Status Codes |
|---|---|---|---|
| `GET` | `/health` | Gateway health, default mode, fail-closed halt state, and per-broker health checks | `200`, `503` (when halted) |
| `GET` | `/v1/capabilities` | Versioned broker capability matrix (`config/capabilities.json`) | `200` |
| `GET` | `/v1/instruments` | Versioned canonical-to-broker instrument catalog (`config/instruments.json`) | `200` |
| `GET` | `/v1/metrics` | Atomic gateway counters (submits, rejects, duplicates, out-of-order fills, reconciliations, halts) | `200` |
| `GET` | `/v1/orders?broker_id=...` | List canonical orders (optionally filtered by broker) | `200` |
| `POST` | `/v1/orders` | Submit canonical `OrderRequest` (idempotent on `client_order_id`) | `201`, `400`, `401`, `403`, `409`, `429`, `503`, `504` |
| `GET` | `/v1/orders/{id}` | Retrieve order by `order_id` or `client_order_id` along with sorted canonical fills | `200`, `404` |
| `POST` | `/v1/orders/{id}/cancel` | Cancel an active order; returns `504` + `CANCEL_PENDING` order on cancel timeout | `200`, `404`, `429`, `504` |
| `POST` | `/v1/webhooks` | Ingest a broker `WebhookEvent` idempotently (deduplicates events & fills; reorders out-of-order fills) | `200`, `400`, `404` |
| `GET` | `/v1/reconcile` | List historical `ReconciliationReport` records | `200` |
| `POST` | `/v1/reconcile?broker_id=...` | Trigger independent reconciliation across one or all brokers | `200` |
| `POST` | `/v1/operator/halt` | Manually trip fail-closed execution halt (`{"reason": "..."}`) | `200` |
| `POST` | `/v1/operator/resume` | Operator-authenticated resume (`{"operator_id": "...", "note": "..."}`) | `200`, `400` |
| `GET` | `/v1/ws` | RFC 6455 WebSocket upgrade streaming real-time `GatewayEvent` JSON frames | `101`, `400` |

## 2. Canonical Domain Contracts

### `OrderRequest`
- `client_order_id` (string, required): Stable client-assigned idempotency key.
- `broker_id` (string, required): Target broker (`mock`, `sandbox-alpha`, `sandbox-beta`).
- `mode` (enum, default `"paper"`): `paper` | `sandbox` | `live` (`live` is rejected by policy).
- `instrument_id` (string, required): Canonical ID (e.g., `EQ:US:AAPL`, `ETF:US:SPY`, `CRYPTO:SPOT:BTC-USD`).
- `side` (enum): `BUY` | `SELL`.
- `order_type` (enum): `MARKET` | `LIMIT` | `STOP_LIMIT` | `STOP`.
- `time_in_force` (enum): `DAY` | `GTC` | `IOC` | `FOK`.
- `quantity` (float64 > 0): Order quantity in canonical instrument units.
- `limit_price` / `stop_price` (float64): Required for `LIMIT` / `STOP_LIMIT` / `STOP`.
- `currency` (string, required): ISO currency matching canonical instrument spec (e.g., `"USD"`).
- `submitted_at_utc` (RFC3339 UTC timestamp).

### Structured Error Taxonomy (`GatewayError`)

| Code | HTTP | Retryable | Trigger |
|---|---|---|---|
| `VALIDATION_ERROR` | `400` | No | Missing/invalid request fields, lot/min-qty violations |
| `UNSUPPORTED_CAPABILITY` | `400` | No | Order type, TIF, asset class, fractional qty, or notional unsupported by broker (rejected pre-dispatch) |
| `SYMBOL_MISMATCH` | `400` | No | Unknown canonical instrument, unmapped broker symbol, or currency mismatch (rejected pre-dispatch) |
| `AUTH_EXPIRED` / `AUTH_INVALID` | `401` | No (rotate secret) | Broker credential missing, revoked, or expired |
| `POLICY_DENIED` | `403` | No | `live` execution attempted or policy limits violated |
| `ORDER_NOT_FOUND` / `BROKER_NOT_FOUND` | `404` | No | Unknown order ID or unregistered broker |
| `IDEMPOTENCY_CONFLICT` | `409` | No | Same `client_order_id` reused with different payload hash |
| `RATE_LIMITED` | `429` | Yes (after `retry_after_ns`) | Local token bucket or upstream 429 cooldown |
| `GATEWAY_HALTED` | `503` | No (operator resume) | Fail-closed circuit tripped by reconciliation drift or operator |
| `UNKNOWN_OUTCOME` | `504` | **NEVER blindly retry** | Submit transport timeout; order marked `SUBMIT_UNKNOWN`, resolve via `/v1/reconcile` |
| `CANCEL_TIMEOUT` | `504` | **NEVER blindly retry** | Cancel timed out; order marked `CANCEL_PENDING`, resolve via `/v1/reconcile` |

## 3. PostgreSQL Schema

Production relational persistence DDL matching `src/storage/Store` is provided in [`docs/schema.sql`](schema.sql). Local reproducible execution uses the append-only JSONL write-ahead `JournalStore` (`src/storage/journal_store.go`).
