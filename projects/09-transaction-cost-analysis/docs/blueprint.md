# Transaction Cost Analysis (TCA)

> Measure execution quality using order, fill, quote, and benchmark data.

**Difficulty:** Intermediate–Advanced  
**Estimated build window:** 4–7 weeks (illustrative, solo development)  
**Suggested stack:** Python, Polars/Pandas, DuckDB/ClickHouse, FastAPI, Plotly/React, pytest  
**Status:** Project blueprint — not yet implemented

## Problem and outcome

Build a reproducible dashboard explaining why an order cost more or less than relevant benchmarks. Make benchmark choice and market-data timing explicit, and separate spread/fees/market impact where the available data permits.

## System architecture

```text
OMS orders + fills    Market quotes + bars
         |                       |
         +---- time alignment ---+
                    |
         Benchmark calculations
                    |
      Cost attribution + aggregation
                    |
         Dashboard / CSV report
```

## Minimum viable product (MVP)

- Ingest parent/child orders, executions, fees, and timestamped quotes.
- Calculate arrival-price shortfall, VWAP deviation, explicit fees, effective spread and fill rate.
- Slice execution quality by broker, symbol, time window, venue and strategy.
- Display missing/stale benchmark flags and confidence notes.
- Export versioned reports with reproducible calculation assumptions.

### Explicit non-goals

- Claiming causal market impact from only coarse bars.
- Comparing venue performance without normalizing order difficulty.
- Mixing buy and sell signed costs incorrectly.

## Services and responsibilities

- `importer`: normalized sample CSV/Parquet loaders.
- `benchmark`: arrival, mid, interval VWAP calculations.
- `attribution`: signed cost and fee breakdowns.
- `report-api`: queries and exports.
- `dashboard`: drilldowns and benchmark overlays.

## Domain model (starting point)

```text
ParentOrder(id, symbol, side, qty, decision_ts, arrival_ts)
Fill(order_id, qty, price, fee, ts, venue)
Quote(symbol, bid, ask, ts, venue)
Benchmark(order_id, name, value, methodology_version)
TcaResult(order_id, shortfall_bps, fees, fill_rate, flags[])
```

Adjust schemas as requirements sharpen. Record timestamps in UTC with venue/event time versus local receive time clearly distinguished, and version decisions/rules where possible.

## API or protocol sketch

These are **illustrative contracts**, not working endpoints.

```text
POST /v1/imports/orders
POST /v1/imports/fills
POST /v1/imports/quotes
GET  /v1/tca/orders/{order_id}?benchmark=arrival_mid
GET  /v1/tca/summary?group_by=venue
GET  /v1/tca/export.csv
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

1. Week 1: fixture dataset, time synchronization and signed-cost formulas.
2. Week 2: core measures and unit tests with manually calculated examples.
3. Week 3–4: aggregations, quality flags and interactive dashboard.
4. Week 5+: TCA comparisons against simulator execution strategies.

## Testing strategy

- **Unit:** validation, state transitions, numeric boundaries and all documented invariants.
- **Integration:** API, state store, message transport and simulator under retries/restarts.
- **Scenario:** missing data, clock skew, stale events, connection loss, duplicate delivery and rejected writes.
- **Replay:** fixed fixtures and seeds produce reproducible outputs; pin versions of dependencies and datasets.
- **Performance:** report realistic load shape, machine details, p50/p99 where relevant, errors and memory usage (do not report fabricated metrics).

## Definition of done

- Signed shortfall is positive for unfavorable fills for both buys and sells.
- Missing arrival quotes are flagged instead of backfilled from future data.
- Report output includes benchmark methodology and dataset versions.
- Sample calculations reconcile manually to within rounding tolerance.

## Metrics to expose

- Implementation shortfall in bps
- Fees and spread cost by strategy
- Fill rate and time-to-fill
- Share of orders with trustworthy benchmarks

## Stretch goals

- Time-weighted interval VWAP and participation analytics.
- Order-difficulty normalization in broker scorecards.
- Link directly to the Smart Order Router project.

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
