# AI Quant Research Agent

> An experiment-orchestrating assistant that tests hypotheses without leaking future data.

**Difficulty:** Advanced  
**Stack:** Python 3.12, FastAPI, Pydantic v2, SQLite (WAL), Prometheus (`prometheus-client`), pytest, ruff  
**Status:** Implemented (Paper / Simulated Research MVP + Stretch Milestones)

> **Safety Notice:** Strictly **PAPER / SIMULATED RESEARCH ONLY**. Live broker connectivity and real-money execution are explicitly disabled in code (`QUANT_AGENT_MODE=PAPER_SIMULATED`). No strategy profitability is implied.

## Problem and outcome

Let an AI research assistant formulate falsifiable trading hypotheses, generate constrained experiment configurations, invoke a deterministic event-driven backtester, critique results with multiple-testing corrections, and publish evidence-linked reproducible research reports. The agent cannot invent performance numbers, execute arbitrary code, or silently cherry-pick top backtests post hoc.

## System architecture

See [docs/architecture.md](docs/architecture.md), [ADR 0001](docs/decisions/0001-deterministic-dsl-and-leakage-barriers.md), and [docs/runbook.md](docs/runbook.md).

```text
Research question
       |
       v
planner (Constrained SignalDefinition DSL + explicit H0)
       |
       v
validator (allowlist, entitlements, checksums, split purge/embargo,
           causal lag, clock skew, staleness, code-injection checks)
       |
       v
runner + backtester (queued sync/async, idempotency, next-open Decimal fills,
                     transaction costs, walk-forward, stationary block bootstrap)
       |
       v
registry (SQLite WAL immutable configs/runs, checksummed artifacts, research memory)
       |
       v
reporter (self-verifying citations to stored RunResult IDs, multiple-testing
          warnings, IS-vs-OOS degradation, failure cases, reproducibility manifest)
       |
       v
Human peer-review sign-off (separated researcher vs reviewer authorization)
```

## Implemented vs. Planned

### Implemented
- **Constrained hypothesis & experiment planner** ([planner.py](src/quant_agent/planner/planner.py)): maps research questions to falsifiable `Hypothesis` records (with explicit null hypothesis `H0`), allowlisted `SignalDefinition` primitives ([signals.py](src/quant_agent/models/signals.py)), and chronologically isolated train/val/test `Experiment` configurations.
- **Pre-execution leakage & integrity validator** ([validator.py](src/quant_agent/validator/validator.py)): versioned validator (`validator_version=1.0.0`) rejecting split overlaps, insufficient purge/horizon gaps, negative feature shifts, `lag_bars < 1`, arbitrary code expressions, non-allowlisted or unentitled datasets, SHA-256 checksum mismatches, point-in-time `available_asof` violations, stale/missing/duplicate bars, clock skew (`receive_time < event_time` or >300s), and exceeded trial budgets.
- **Deterministic event-driven backtester & fixed-point cost model** ([engine.py](src/quant_agent/backtest/engine.py), [costs.py](src/quant_agent/backtest/costs.py)): strictly causal bar-by-bar loop (signals on close `t` execute no earlier than `t + lag_bars` open), integer share quantities, `Decimal` money/price/fee arithmetic (commission, half-spread, participation market impact, short borrow fee), and optional sequential walk-forward folds.
- **Statistical & multiple-testing library** ([stats.py](src/quant_agent/backtest/stats.py)): Sharpe, Sortino, max drawdown, Calmar, two-sided t-stat/p-value, Politis-Romano stationary block bootstrap 95% CIs, Bonferroni, Benjamini-Hochberg FDR, Deflated Sharpe Ratio, and in-sample vs. out-of-sample degradation.
- **Immutable registry, artifacts & research memory** ([store.py](src/quant_agent/registry/store.py)): SQLite WAL persistence enforcing immutability (`ImmutabilityError`) on hypotheses, validated configs, and runs; SHA-256 checksummed JSON artifacts (`config`, `metrics`, `trades`, `equity_curve`, `manifest`); audit event log; and cross-hypothesis research memory tracking both failed and candidate ideas.
- **Queued runner & reproducibility verification** ([runner.py](src/quant_agent/runner/runner.py)): sync & async `ThreadPoolExecutor` jobs with idempotency keys and `reproduce_run()` replay verification asserting bit-for-bit `trades_digest` and metric equality within tolerance.
- **Evidence-linked reporter** ([reporter.py](src/quant_agent/reporter/reporter.py)): refuses to generate reports without stored runs; verifies every numeric citation against immutable `RunResult` records; attaches multiple-testing warnings, failure cases, caveats, and a `ReproducibilityManifest`.
- **FastAPI API, telemetry & paper workbench UI** ([app.py](src/quant_agent/api/app.py), [telemetry.py](src/quant_agent/metrics/telemetry.py), [index.html](src/quant_agent/web/index.html)): separated researcher/reviewer authorization, JSON (`/v1/metrics`) + Prometheus (`/metrics`) observability, and an interactive paper workbench.
- **Unit, integration & adversarial scenario tests** ([tests/](tests/)): 35 tests covering causality invariance, leakage attacks, stale data, clock skew, missing/duplicate bars, storage disconnect/read-only faults, async idempotency, replay verification, and peer-review sign-off.

### Planned (Future Extensions)
- External LLM provider adapter implementing `PlannerBackend` (with strict JSON-schema decoding into `SignalDefinition`).
- Distributed worker queue backend (Celery/RQ + Redis) and Parquet/DuckDB market-data catalog adapters for licensed institutional feeds.
- Vector similarity search over historical research memory and automated experiment cost-budget scheduling.

## API endpoints

```text
GET  /                                          Paper Research Workbench UI
GET  /health                                    Service health & PAPER_SIMULATED mode
GET  /v1/metrics                                JSON service metrics snapshot
GET  /metrics                                   Prometheus metrics scrape endpoint
GET  /v1/research/datasets                      Allowlisted datasets & SHA-256 checksums
GET  /v1/research/memory                        Research memory (passed & failed hypotheses)
POST /v1/research/hypotheses                    Formulate hypothesis + proposed experiment
GET  /v1/research/hypotheses                    List hypotheses
GET  /v1/research/hypotheses/{hypothesis_id}    Get hypothesis
POST /v1/research/experiments                   Create experiment revision for a hypothesis
GET  /v1/research/experiments/{experiment_id}   Get experiment
POST /v1/research/experiments/{id}/validate     Run pre-execution leakage & integrity checks
POST /v1/research/experiments/{id}/run          Execute (sync or async + idempotency_key)
GET  /v1/research/jobs/{job_id}                 Check async job status
GET  /v1/research/runs/{run_id}                 Get immutable RunResult
GET  /v1/research/runs/{run_id}/artifacts       Retrieve checksummed run artifacts & manifest
POST /v1/research/runs/{run_id}/reproduce       Re-run pinned experiment & verify replay
POST /v1/research/reports/{hypothesis_id}/generate  Generate evidence-linked report
GET  /v1/research/reports/{hypothesis_id}       Get latest evidence-linked report
POST /v1/research/reports/{hypothesis_id}/review    Submit peer-review sign-off (reviewer role)
```

## Quick start (verified commands)

```bash
make setup             # Create Python 3.12 .venv and install package + dev deps
make lint              # Run ruff check on src, tests, scripts
make test              # Run 24 unit tests
make integration-test  # Run 11 integration + adversarial/fault scenario tests
make test-all          # Run all 35 tests
make demo              # Run end-to-end paper walkthrough + leakage/stale/disconnect failure demos
make benchmark         # Run reproducible latency & memory benchmark
make serve             # Start FastAPI server + Paper UI on http://127.0.0.1:8014
```

### Example HTTP request

```bash
curl -s -X POST http://127.0.0.1:8014/v1/research/hypotheses \
  -H "Content-Type: application/json" \
  -d @examples/abnormal_volume_hypothesis.json | python3 -m json.tool
```

## Metrics exposed

Exposed via both `GET /v1/metrics` (JSON) and `GET /metrics` (Prometheus):
- `quant_agent_reproducibility_rate` — experiment reproducibility rate (`0.0`–`1.0`)
- `quant_agent_rejected_experiments_total{reason}` — rejected invalid/leaky experiments by error code
- `quant_agent_research_cycle_seconds` — research execution cycle time histogram (`p50`/`p99` in JSON)
- `quant_agent_is_vs_oos_sharpe_degradation` — out-of-sample degradation versus in-sample Sharpe

## Verified test & benchmark evidence

- **Lint (`make lint`):** `ruff check src tests scripts` — 0 errors.
- **Test suite (`make test-all`):** 35 passed (24 unit, 4 integration, 7 adversarial/fault scenario tests).
- **Reproducible benchmark (`make benchmark`):**
  - Machine: macOS Darwin 25.5.0 (`arm64`), Python `3.12.15`
  - Workload: 12 full pipelines (8 symbols, ~1,042 daily bars/symbol, train/val/test + 250-resample stationary block bootstrap + SQLite/JSON artifact persistence)
  - Errors: `0` | Peak traced Python heap: `44.00 MiB`
  - Validate: `p50 = 82.31 ms` | `p99 = 93.26 ms`
  - Backtest + Save Artifacts: `p50 = 1430.93 ms` | `p99 = 1510.23 ms`
  - Report + Citation Verification: `p50 = 2.05 ms` | `p99 = 2.66 ms`

## Repository layout

```text
14-ai-quant-research-agent/
├── README.md
├── LICENSE
├── Makefile
├── pyproject.toml
├── Dockerfile
├── docker-compose.yml
├── .github/workflows/ci.yml
├── docs/
│   ├── architecture.md
│   ├── decisions/
│   │   └── 0001-deterministic-dsl-and-leakage-barriers.md
│   └── runbook.md
├── examples/
│   ├── abnormal_volume_hypothesis.json
│   ├── momentum_walkforward_experiment.json
│   └── leaky_experiment_rejected.json
├── scripts/
│   ├── demo.py
│   └── benchmark.py
├── src/quant_agent/
│   ├── api/         # FastAPI app, schemas, role auth
│   ├── backtest/    # Deterministic causal engine, Decimal costs, stats & bootstrap
│   ├── data/        # Allowlisted DatasetCatalog & seeded OHLCV simulator
│   ├── metrics/     # Prometheus & JSON service telemetry
│   ├── models/      # Domain models & constrained SignalDSL
│   ├── planner/     # Hypothesis & leakage-safe experiment planner
│   ├── registry/    # Immutable SQLite store, artifacts, audit log, research memory
│   ├── reporter/    # Evidence-linked report generator & citation verifier
│   ├── runner/      # Sync/async job queue, idempotency, replay verifier
│   ├── validator/   # Pre-execution leakage, entitlement, & data-hygiene gate
│   └── web/         # Paper/Simulated Research Workbench HTML UI
└── tests/
    ├── unit/
    ├── integration/
    └── scenarios/
```

## License

MIT — see [LICENSE](LICENSE).
