# Production Algorithmic Trading Platform

Two deterministic strategies trade against a paper broker. The platform reserves exposure before dispatch, journals each order in SQLite, and reconciles fills and cash against the broker. Shadow mode records strategy decisions without submitting orders. The supplied fixtures exercise paper trading and recovery. Live trading is disabled by default.

## Getting started

Setup uses Python 3.12 and the uv package available to `python3 -m uv`.

From the repository root, enter the project directory and run the following commands.

```bash
cd projects/16-production-algorithmic-trading-platform
./scripts/setup.sh
.venv/bin/python scripts/demo.py
./scripts/validate.sh
```

The demo prints the paper order and ledger results. Validation also runs the fault drills and a 2,000-tick soak. To inspect the operator console, run the following command and open `http://127.0.0.1:8080`.

```bash
.venv/bin/patp --config config/platform.paper.json serve
```

Run `.venv/bin/patp --config config/platform.paper.json check-live-gate` to inspect the execution policy. With the supplied paper configuration the command exits with a rejection.

## How the platform works

| Workstream | Implementation |
|---|---|
| **Canonical Market & Order Event Contracts** | Versioned Pydantic v2 contracts (`SCHEMA_VERSION = "1.0.0"`) in `src/trading_platform/domain/` enforcing UTC timestamps, exact `Decimal` precision, explicit currencies, structured errors, and fail-closed `ExecutionPolicyConfig`. |
| **Strategy Runtime & Market Data Guard** | `MarketDataGuard` enforces feed entitlements, staleness (`STALE_FEED`), clock skew (`CLOCK_SKEW_DETECTED`), and monotonic sequencing; `StrategyScheduler` executes deterministic `MomentumCrossoverStrategy` and `MeanReversionStrategy` plus zero-dispatch **Shadow Mode**. |
| **Deterministic Risk Gate & Kill Switch** | 10 deterministic pre-trade rules (`R01`–`R10`), atomic pre-dispatch exposure reservations (**retained on `UNKNOWN_OUTCOME`**), scoped kill switches (`GLOBAL`/`STRATEGY`/`SYMBOL`), and Maker-Checker dual-control release. |
| **Idempotent OMS & Double-Entry Ledger** | SQLite WAL persistence with unique `idempotency_key` + SHA-256 payload fingerprinting, strict state transition machine, cancel/fill race safety, crash/restart recovery (`recover_after_restart`), and strictly balanced double-entry accounting (`debits == credits`). |
| **Broker Adapter & Reconciliation** | Clean `BrokerAdapter` port with deterministic `PaperBrokerAdapter` + fault injection hooks; `BrokerReconciler` resolves `UNKNOWN_OUTCOME` orders, reconciles cash/positions/orders against broker snapshots/fixtures, and halts on critical breaks. |
| **Observability, Deployment & Operator Console** | OTel-compatible `TelemetryRecorder` (counters, gauges, p50/p95/p99 latency histograms, spans), SHA-256 hash-chained audit trail, RBAC REST API, `patp` CLI, zero-CDN web console, `Dockerfile`, and `docker-compose.yml`. |

## Project layout

```text
16-production-algorithmic-trading-platform/
├── plan.md                        # Implementation plan (M1-M4 & acceptance checklist)
├── pyproject.toml                 # Package metadata, dependencies, ruff & pytest config
├── uv.lock                        # Reproducible dependency lockfile
├── Dockerfile / docker-compose.yml
├── config/                        # Versioned paper/shadow configs, live gate template, fixture manifest (no secrets)
├── data/fixtures/                 # Synthetic paper feed, entitled shadow feed, broker reconciliation fixture
├── docs/                          # Architecture, ADRs (001-003), schema.sql, API/CLI docs, runbook, live gate, unsupported features
├── src/trading_platform/          # domain/, storage/, observability/, ledger/, risk/, oms/, broker/, strategy/, reconciliation/, api/, web/, platform.py, cli.py
├── scripts/                       # setup.sh, validate.sh, demo.py, fault_drills.py, soak_and_benchmark.py
├── tests/                         # unit/, integration/, scenarios/ (all 8 mandatory failure scenarios)
└── reports/                       # M1-M4 milestone reports, acceptance checklist, raw evidence/
```

## Documentation and evidence

- [Architecture & State Machine Diagrams](docs/architecture.md)
- [API, CLI & PostgreSQL-Compatible Schema](docs/api_and_schema.md) ([schema.sql](docs/schema.sql))
- [Operator Runbook & 8-Scenario Playbooks](docs/runbook.md)
- [Live Rollout Gate & Security/Risk Review](docs/live_rollout_gate.md)
- [Explicit Unsupported Features & Non-Goals](docs/unsupported_features.md)
- [ADRs](docs/adr/ADR-001-clean-room-contracts-and-repo-audit.md) | [ADR-002](docs/adr/ADR-002-deterministic-risk-and-unknown-outcome-reservations.md) | [ADR-003](docs/adr/ADR-003-storage-and-deployment-boundaries.md)
- [Milestone Reports](reports/milestone_m1_foundation.md) | [M2](reports/milestone_m2_core.md) | [M3](reports/milestone_m3_integration.md) | [M4](reports/milestone_m4_hardening_and_review.md)
- [Acceptance Checklist & Raw Evidence](reports/acceptance_checklist.md)
