# Broker Reliability & Reconciliation

> Detect and resolve order, fill, cash, and position divergence across internal and broker records.

**Difficulty:** Advanced  
**Runtime:** Go 1.27.1 (zero external dependencies; standard library HTTP, fixed-point `math/big`, Prometheus text exposition, embedded Operator UI)  
**Status:** MVP implemented (paper/simulated connectors by default; verified with race-enabled unit, integration, scenario, and benchmark suites)

## Problem and outcome

A reliability layer that compares internal OMS/ledger trading state against broker statements and updates as of an explicit UTC cutoff. It surfaces typed discrepancies with immutable original evidence, controlled operator resolution workflows, Prometheus SLIs, and operational incident runbooks.

## System architecture

```text
Internal OMS / ledger          Broker API / statements
(read-only connector)            (read-only connector)
         |                               |
         +------ identity-mapper --------+
                         |
                  reconcile-core
                         |
                   Diff classifier
                         |
               issues.Store (immutable evidence + audit log)
                         |
        +----------------+----------------+
        |                                 |
   REST API & Operator UI        Prometheus /metrics
```

See [docs/architecture.md](docs/architecture.md), [ADR 0001](docs/decisions/0001-immutable-evidence-and-fixed-point-arithmetic.md), and [docs/runbook.md](docs/runbook.md).

## Explicit non-goals (enforced by construction)

- **Never overwrite ledger history to force a match:** Internal/broker connectors expose strictly read-only `List*` methods, and `issues.Store` keeps original `IssueEvidence` immutable across reruns and resolutions.
- **Never automatically place offsetting trades:** No execution or order-submission endpoints exist in connectors or remediation workflows.
- **Never treat pending/settling balances as final settled cash:** `CashSnapshot` strictly separates `settled_cash` and `pending_cash`; settled breaks are evaluated exclusively against `settled_cash`.

## Implemented vs. Planned

### Implemented
- **Domain & Fixed-Point Arithmetic** ([src/domain/decimal.go](src/domain/decimal.go), [src/domain/models.go](src/domain/models.go)): Immutable scale-$10^8$ `Decimal` backed by `*big.Int` (no floating-point currency math), UTC dual timestamps (`event_time` vs `receive_time`), versioned `ReconPolicy`, and typed discrepancy models.
- **Read-Only Paginated Connectors & Fault Injection** ([src/connectors/connector.go](src/connectors/connector.go), [src/connectors/simulator.go](src/connectors/simulator.go)): Cursor pagination (`FetchAllPages`) with bounded retries, simulated paper fixtures ([examples/fixtures/paper_day.json](examples/fixtures/paper_day.json)), and configurable `FaultProfile` (disconnects, rate limits, dropped webhooks, duplicate delivery, stale snapshots, clock skew, late fills).
- **Identity & Normalization Mapper** ([src/identity/mapper.go](src/identity/mapper.go)): Symbol canonicalization (`AAPL.US` -> `AAPL`, `BRK-B`/`BRKB` -> `BRK.B`), ISO currency/side normalization, UTC timestamp enforcement, and bidirectional `client_order_id` <-> `broker_order_id` <-> `broker_exec_id` linking.
- **Deterministic Reconciliation Core** ([src/reconcile/engine.go](src/reconcile/engine.go)): As-of cutoff filtering, SHA-256 `InputDigest`, content-addressed run/issue IDs, and detection of `missing_broker_fill`, `missing_ledger_fill`, `duplicate_broker_fill`, `duplicate_ledger_fill`, `late_fill`, `fill_mismatch`, `missing_broker_order`, `missing_ledger_order`, `order_mismatch`, `position_break`, `settled_cash_break`, `pending_cash_break`, `stale_snapshot`, and `clock_skew_anomaly`.
- **Exception Queue & Durable Store** ([src/issues/store.go](src/issues/store.go)): Lifecycle state machine (`open`, `investigating`, `escalated`, `resolved`, `waived`), owner assignment, immutable original evidence, append-only audit history, and crash-safe JSONL persistence with reload on restart.
- **Scheduler & Prometheus Metrics** ([src/scheduler/runner.go](src/scheduler/runner.go), [src/metrics/prometheus.go](src/metrics/prometheus.go)): On-demand & periodic reconciliation runs exposing `recon_unreconciled_issues`, `recon_monetary_exposure`, `recon_position_drift_abs`, `recon_mttd_seconds`, `recon_mttr_seconds`, `recon_late_fill_arrival_seconds`, `recon_runs_total`, and `recon_connector_errors_total`.
- **REST API, AuthN/AuthZ Boundary & Operator UI** ([src/api/server.go](src/api/server.go), [src/api/auth.go](src/api/auth.go), [src/operatorui/index.html](src/operatorui/index.html)): All `/v1/reconciliation/*` endpoints, constant-time API key authentication & RBAC authorization (`viewer`, `operator`, `admin`), `/metrics`, `/healthz`, and an offline-capable Operator Exception Dashboard visibly labeled **PAPER / SIMULATED**.
- **Tests, Runbooks, CI & Demos**:
  - Unit tests: [tests/unit/domain_and_identity_test.go](tests/unit/domain_and_identity_test.go)
  - Integration tests: [tests/integration/api_and_persistence_test.go](tests/integration/api_and_persistence_test.go)
  - Scenario & fault-injection tests: [tests/scenarios/scenarios_test.go](tests/scenarios/scenarios_test.go)
  - Performance benchmarks: [tests/performance/benchmark_test.go](tests/performance/benchmark_test.go)
  - Incident runbooks: [docs/runbook.md](docs/runbook.md)
  - Walkthrough & failure demos: [scripts/demo.sh](scripts/demo.sh), [scripts/failure_demo.sh](scripts/failure_demo.sh)
  - CI & Docker: [.github/workflows/ci.yml](.github/workflows/ci.yml), [Dockerfile](Dockerfile), [docker-compose.yml](docker-compose.yml)

### Planned (Stretch Goals)
- Live PostgreSQL storage driver alongside the durable JSONL store.
- Streaming Kafka/NATS ingestion adapter for continuous intra-day reconciliation.
- Multi-broker reconciliation with exchange business-day settlement calendars.
- Corporate actions (splits, dividends, spinoffs) and T+1 settlement lifecycle tracking.
- Automated journal-correction proposal export for accounting approval queues.

## API endpoints

```text
POST /v1/reconciliation/runs
  {"account_id":"a-1","asof":"2026-01-31T16:00:00Z","policy_version":"v1.0.0"}

GET  /v1/reconciliation/runs
GET  /v1/reconciliation/runs/{id}
GET  /v1/reconciliation/issues?status=open&account_id=a-1&severity=critical
GET  /v1/reconciliation/issues/{id}
POST /v1/reconciliation/issues/{id}/assign
  {"owner":"ops-alice","actor":"ops-lead"}
POST /v1/reconciliation/issues/{id}/state
  {"state":"investigating","note":"Checking broker drop-copy","actor":"ops-alice"}
POST /v1/reconciliation/issues/{id}/resolve
  {"resolution":"late_broker_event","evidence":"Confirmed in EOD drop; ticket OPS-412","actor":"ops-alice"}
GET  /v1/reconciliation/policies
GET  /metrics
GET  /healthz
GET  /   (Operator Exception Dashboard UI)
```

## Quick start (verified commands)

```bash
make setup             # Verify Go toolchain and tidy module
make lint              # gofmt check + go vet ./...
make test              # Run unit, integration, and scenario tests with -race
make integration-test  # Run API, persistence, auth boundary, and scheduler integration tests
make bench             # Run 1k and 10k fill benchmarks + latency distribution report
make demo              # Run end-to-end paper/simulated reconciliation walkthrough
make failure-demo      # Run broker disconnect, retry, and stale-snapshot failure demo
make serve             # Start HTTP API + Operator UI on http://localhost:8080 (paper/simulated)
```

### Optional API key authentication

By default `make serve` runs in local paper-demo mode. To enable constant-time API key authentication and RBAC enforcement, inject credentials via environment variables (never commit secrets):

```bash
RECON_OPERATOR_API_KEY="your-operator-secret" \
RECON_VIEWER_API_KEY="your-viewer-secret" \
make serve
```

## Verified test and benchmark results

Measured on `darwin/arm64` (Apple M5, 10 CPUs, Go `1.27.1`):

- **Test suite (`make test` with `-race`):** 16/16 tests passing across `tests/unit` (6), `tests/integration` (3), and `tests/scenarios` (7).
- **Performance (`make bench`):**
  - Synthetic 1,000-fill book (~2.5% injected breaks, 50 runs): **p50 = 3.16 ms**, **p99 = 11.22 ms**, **~3.91 MB alloc/run**.
  - `BenchmarkReconcile1000Fills-10`: **~3.00 ms/op** (3.83 MB/op, 72,905 allocs/op).
  - `BenchmarkReconcile10000Fills-10`: **~29.49 ms/op** (35.23 MB/op, 727,001 allocs/op).

## Repository layout

```text
.
├── README.md
├── Makefile
├── go.mod
├── Dockerfile
├── docker-compose.yml
├── .github/workflows/ci.yml
├── docs/
│   ├── architecture.md
│   ├── decisions/
│   │   └── 0001-immutable-evidence-and-fixed-point-arithmetic.md
│   └── runbook.md
├── src/
│   ├── api/
│   ├── cmd/recon/
│   ├── connectors/
│   ├── domain/
│   ├── identity/
│   ├── issues/
│   ├── metrics/
│   ├── operatorui/
│   ├── reconcile/
│   └── scheduler/
├── tests/
│   ├── unit/
│   ├── integration/
│   ├── scenarios/
│   └── performance/
├── examples/
│   ├── fixtures/paper_day.json
│   └── policies/default_policy.json
└── scripts/
    ├── demo.sh
    └── failure_demo.sh
```

## Safety, data and operational boundaries

- **Paper/simulated by default.** No real-money broker connectivity is enabled.
- Keep any future broker API keys in a secret manager or environment variables — never in source control, logs, or UI code.
- Authentication (`Authenticator`), authorization (`Authorizer`), reconciliation (`Engine`), and storage (`Store`) are isolated behind independently testable interfaces.
- All financial quantities, prices, fees, and cash balances use fixed-point `domain.Decimal` integer arithmetic.
