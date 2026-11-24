# Architecture — AI Quant Research Agent

> **Boundary:** Strictly **PAPER / SIMULATED RESEARCH ONLY**. No live broker connectivity or capital allocation exists in this system.

## High-Level Data Flow

```text
Research Question
       |
       v
+------------------------------+
| planner (ResearchPlanner)    |  Constrained SignalDefinition DSL + explicit H0
+------------------------------+  (No arbitrary code, no invented performance numbers)
       |
       v
+------------------------------+
| validator                    |  Leakage & integrity gate (validator_version=1.0.0):
| (ExperimentValidator)        |  - Split isolation + purge/horizon gaps
+------------------------------+  - Negative shift / lag < 1 / code injection checks
       |                          - Allowlist, entitlement tier, SHA-256 checksum
       |                          - Point-in-time available_asof <= asof_timestamp_utc
       |                          - Clock skew, stale bars, missing gaps, duplicates
       v
+------------------------------+
| runner + backtester          |  Queued sync/async worker + idempotency key
| (EventDrivenBacktester)      |  - Strictly causal bar-by-bar loop (signal[t] -> open[t+lag])
+------------------------------+  - Fixed-point Decimal cash/equity/fees, integer shares
       |                          - Stationary block bootstrap + Bonferroni/BH/DSR
       v
+------------------------------+
| registry (SQLite + Disk)     |  Immutable hypotheses, validated configs, RunResults,
| (ExperimentRegistry)         |  checksummed JSON artifacts, audit log, research memory
+------------------------------+
       |
       v
+------------------------------+
| reporter (ResearchReporter)  |  Evidence-linked report generator:
+------------------------------+  - Every metric verified against stored RunResult IDs
       |                          - Trial count, holdout window, costs, failure cases,
       v                            IS-vs-OOS degradation, ReproducibilityManifest
+------------------------------+
| Human Peer Review            |  Separated reviewer token/role (`X-Role: reviewer`)
+------------------------------+
```

## Service Boundaries

| Component | Package | Responsibility |
| :--- | :--- | :--- |
| `planner` | `quant_agent.planner` | Translates natural-language research questions into falsifiable hypotheses and leakage-safe experiment schemas. |
| `validator` | `quant_agent.validator` | Enforces allowlisted datasets, entitlements, checksums, chronological isolation, point-in-time availability, and data hygiene before any run. |
| `backtest` | `quant_agent.backtest` | Executes strictly causal bar-by-bar paper simulations with fixed-point `Decimal` transaction costs and dependence-aware statistical tests. |
| `runner` | `quant_agent.runner` | Manages queued async/sync jobs with idempotency keys, persists immutable artifacts, and verifies pinned reproducibility replays. |
| `registry` | `quant_agent.registry` | SQLite WAL store enforcing immutability on runs/configs, audit trail logging, and cross-hypothesis research memory. |
| `reporter` | `quant_agent.reporter` | Synthesizes evidence-linked reports with self-verifying citations, multiple-testing warnings, and reproducibility manifests. |
| `metrics` | `quant_agent.metrics` | Tracks reproducibility rate, rejected leaky/invalid experiments, research cycle time, and IS-vs-OOS degradation via JSON and Prometheus. |
| `api` | `quant_agent.api` | FastAPI REST layer with separated researcher and reviewer auth boundaries plus a paper research workbench UI. |

## Temporal & Money Arithmetic Invariants

1. **UTC & Clock Separation:** All timestamps are timezone-aware UTC. Market bars distinguish `event_time_utc` (venue bar close) from `receive_time_utc` (local ingest). Negative skew (`receive < event`) or excessive skew (`> 300s`) fails validation.
2. **Strict Causality:** Signals computed from bar `t` close cannot execute earlier than bar `t + lag_bars` open (`lag_bars >= 1`, `feature_shift >= 0`).
3. **Fixed-Point Money:** Prices, fill prices, notional, commissions, spread costs, market impact, borrow fees, cash, and PnL use `decimal.Decimal` with explicit cent/tick quantization; order sizes are integer share quantities.
