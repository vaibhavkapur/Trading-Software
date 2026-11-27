# API, CLI, and Event Contract Reference

## 1. Canonical Event Contracts (`SCHEMA_VERSION = "1.0.0"`)

All domain events live in `src/trading_platform/domain/events.py` and enforce:
- Timezone-aware **UTC timestamps** (`event_time_utc`, `available_time_utc`, `occurred_at_utc`)
- Exact **`Decimal`** precision for prices, quantities, commissions, and cash balances
- Explicit **ISO currency** (`USD`) and stable identifiers (`tick_id`, `signal_id`, `idempotency_key`, `client_order_id`, `broker_order_id`, `fill_id`, `entry_id`, `reconciliation_id`, `event_id`)
- Explicit **structured errors** (`src/trading_platform/domain/errors.py`) with `error_code`, `message`, `recoverable`, and `details`.

## 2. Operator REST API (`src/trading_platform/api/server.py`)

Authorization uses `X-Actor-Id` and `X-Actor-Role` (`VIEWER`, `AUDITOR`, `OPERATOR`, `RISK_OFFICER`, `COMPLIANCE`, `CTO`).

| Method | Endpoint | Allowed Roles | Description |
|---|---|---|---|
| `GET` | `/api/health` | Any | Liveness, execution mode, broker connectivity, ledger balance check |
| `GET` | `/api/status` | Any | Full operational snapshot (ledger, orders, reservations, kill switches, reconciliation, telemetry) |
| `GET` | `/api/orders` | Any | List all OMS orders and states |
| `GET` | `/api/fills` | Any | List all applied fills |
| `GET` | `/api/reservations` | Any | List active and historical risk exposure reservations |
| `GET` | `/api/ledger` | Any | Trial balance, cash balance, and positions |
| `GET` | `/api/reconciliations` | Any | Historical reconciliation reports and breaks |
| `GET` | `/api/audit` | Any | Recent hash-chained audit events and chain verification result |
| `GET` | `/api/metrics` | Any | Telemetry counters, gauges, latency histograms, and recent trace spans |
| `GET` | `/api/live-gate` | Any | Fail-closed live enablement verification gaps |
| `POST` | `/api/orders` | `OPERATOR`, `CTO` | Submit manual paper `OrderIntent` through Risk + OMS |
| `POST` | `/api/orders/{cid}/cancel` | `OPERATOR`, `RISK_OFFICER`, `CTO` | Request cancel of open order |
| `POST` | `/api/kill-switch/engage` | `OPERATOR`, `RISK_OFFICER`, `COMPLIANCE`, `CTO` | Engage scoped kill switch and cancel open orders |
| `POST` | `/api/kill-switch/release` | `OPERATOR`, `RISK_OFFICER`, `COMPLIANCE`, `CTO` | Release kill switch (enforces Maker-Checker dual release when required) |
| `POST` | `/api/reconcile` | `OPERATOR`, `RISK_OFFICER`, `COMPLIANCE`, `CTO` | Run broker reconciliation |

## 3. CLI (`patp`)

```bash
.venv/bin/patp --config config/platform.paper.json status
.venv/bin/patp --config config/platform.paper.json check-live-gate
.venv/bin/patp --config config/platform.paper.json reconcile
.venv/bin/patp --config config/platform.paper.json kill-engage --reason "Incident drill" --dual-release
.venv/bin/patp --config config/platform.paper.json kill-release --reason "Cleared" --actor op-1 --role OPERATOR --secondary-approver risk-1 --secondary-role RISK_OFFICER
.venv/bin/patp --config config/platform.paper.json serve --host 127.0.0.1 --port 8080
```

## 4. Relational Database Schema

See [schema.sql](schema.sql) for the PostgreSQL-compatible DDL corresponding directly to the SQLite WAL tables in `src/trading_platform/storage/sqlite_store.py`.
