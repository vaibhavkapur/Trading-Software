# Market Surveillance Engine

> Streaming alerts for suspicious order-flow patterns with case-management evidence.

**Difficulty:** Advanced  
**Estimated build window:** 6–10 weeks (illustrative, solo development)  
**Suggested stack:** Python/Go, Kafka, Flink or simple stream processors, PostgreSQL, ClickHouse, React, scikit-learn optional  
The Go implementation processes simulated order flow, opens cases, and replays the journal. The original design and milestones appear below.

## Quick start

With Go 1.27.1 and Make installed, run the following commands from this directory.

```bash
make setup
make demo
make test-all
```

## Problem and outcome

Monitor simulated orders and trades for suspicious patterns such as high cancellation ratios, potential layering, and wash-trade-like behavior. Treat alerts as **investigation leads**, not proof of manipulation.

## System architecture

```text
Orders + cancels + trades
             |
Normalized event stream
             |
Windowed feature processor
             |
Rules / anomaly detector
             |
Explainable alerts
             |
Case dashboard + evidence export
```

## Minimum viable product (MVP)

- Ingest timestamped order submissions, amendments, cancels and executions.
- Rolling order-to-trade ratio and cancellation-rate features.
- Synthetic scenarios illustrating layering-like and wash-trade-like patterns.
- Rules with explicit thresholds, explainable evidence and severity.
- Case management: triage, annotate, resolve and export audit trail.

### Explicit non-goals

- Autonomously accusing traders of misconduct.
- Training ML on synthetic labels and reporting it as proven real-world performance.
- Treating one unusual order as conclusive manipulation.

## Services and responsibilities

- `event-normalizer`: canonical accounts, venues, orders and trades.
- `feature-pipeline`: event-time windows and watermark handling.
- `rules`: versioned detectors and alert suppression.
- `cases`: analyst workflow and evidence snapshotting.
- `evaluation`: synthetic positive/negative scenarios and alert statistics.

## Domain model (starting point)

```text
OrderEvent(account_id, order_id, symbol, action, side, qty, price, ts)
TradeEvent(trade_id, buy_account, sell_account, qty, price, ts)
FeatureWindow(key, start, end, cancel_rate, order_trade_ratio)
Alert(id, rule_id, event_refs[], severity, explanation)
Case(id, alert_ids[], status, analyst_notes, audit_events[])
```

Adjust schemas as requirements sharpen. Record timestamps in UTC with venue/event time versus local receive time clearly distinguished, and version decisions/rules where possible.

## API or protocol sketch

These are **illustrative contracts**, not working endpoints.

```text
POST /v1/sim/scenarios/layering_like
GET  /v1/alerts?symbol=XYZ&status=open
POST /v1/alerts/{id}/cases
POST /v1/cases/{id}/notes
GET  /v1/cases/{id}/evidence
```

## Suggested repository layout

```text
.
├── README.md
├── docs/
│   ├── architecture.md
│   ├── decisions/
│   └── runbook.md
├── src/
├── tests/
│   ├── unit/
│   ├── integration/
│   └── scenarios/
├── examples/
├── scripts/
├── docker-compose.yml
└── .github/workflows/ci.yml
```

> This README is a **build specification**, not a claim that the implementation already exists. Replace commands, benchmarks, screenshots and integrations with verified ones as code is added.

## Getting started (implementation checklist)

1. Create the repository using the layout above, choose a package/module name, and pin runtime versions.
2. Add a `Makefile` or task runner with `setup`, `lint`, `test`, `integration-test`, and `demo` targets.
3. Create a local simulator/fixture data source; keep credentials out of source control.
4. Implement the smallest end-to-end vertical slice before optimizing throughput or adding AI.
5. Add CI checks, deterministic replay fixtures, service-level metrics and an architecture decision record.

Example target interface (the targets are **proposed**, not implemented):

```bash
make setup
make lint
make test
make integration-test
make demo
```

## Milestone roadmap

1. Week 1–2: normalized fixtures, event-time windows and baseline detector.
2. Week 3–4: synthetic scenario generator, multiple rules and dedup.
3. Week 5–6: case workflow, audit trace and evaluation set.
4. Week 7+: drift dashboards, operator review and privacy controls.

## Testing strategy

- **Unit:** validation, state transitions, numeric boundaries and all documented invariants.
- **Integration:** API, state store, message transport and simulator under retries/restarts.
- **Scenario:** missing data, clock skew, stale events, connection loss, duplicate delivery and rejected writes.
- **Replay:** fixed fixtures and seeds produce reproducible outputs; pin versions of dependencies and datasets.
- **Performance:** report realistic load shape, machine details, p50/p99 where relevant, errors and memory usage (do not report fabricated metrics).

## Definition of done

- Each alert includes evidence event IDs, window bounds, rule version and thresholds.
- Out-of-order events are processed under documented watermark policies.
- Synthetic controls let you estimate false-positive rates.
- Analyst actions and case changes are auditable.

## Metrics to expose

- Alerts per million events
- Synthetic precision/recall with limitations disclosed
- Event-to-alert latency
- Analyst resolution and duplicate-alert rates

## Stretch goals

- Graph-based related-account detection with permissioned data.
- Supervised model comparison versus rules on curated datasets.
- Risk-team dashboard with evidence replay.

## Safety, data and operational boundaries

- **Paper/simulated by default.** No real-money connectivity is enabled by this specification.
- Use only authorized market feeds and broker APIs; comply with applicable market-data licenses, broker terms, and local trading rules before any production deployment.
- Keep API keys in a secret manager or environment injection, never in prompts, logs, sample commits or browser code.
- Separate authentication, authorization, pre-trade risk and order execution into independently testable boundaries.
- Use integer ticks/lot quantities or fixed-point decimals for money and order arithmetic; avoid floating-point currency calculations in the execution path.
- Treat all examples and performance figures as illustrative until supported by test results; no strategy profitability is implied.

## Demo and portfolio deliverables

- A short screen capture or terminal walkthrough using **simulated** data and visibly labeled paper activity.
- An architecture diagram and brief ADR explaining one difficult engineering trade-off.
- A failure demo showing how the system behaves when a dependency disconnects or data is stale.
- A test report with pass/fail evidence and a reproducible benchmark methodology where applicable.
- A clear section titled **Implemented vs. Planned** once development begins, with links to code and tests.

## License

Choose a license before publishing; MIT or Apache-2.0 are common for open-source software, subject to dependency and data-license compatibility.
