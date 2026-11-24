# Event-Driven Backtesting Engine

> Simulate strategies with realistic fills, fees, slippage, and deterministic replay.

**Difficulty:** Advanced  
**Estimated build window:** 6–10 weeks (illustrative, solo development)  
**Suggested stack:** Python 3.12+, pandas/Polars, PyArrow/Parquet, optional Rust fill simulator, DuckDB, pytest  
**Status:** Project blueprint — not yet implemented

## Problem and outcome

Design a backtesting engine in which the strategy sees only the data available at each simulated timestamp. Keep strategy interfaces compatible with paper trading while explicitly modeling frictions and uncertainty.

## System architecture

```text
Historical events -> Simulated clock
                          |
                         Strategy
                          |
                      Order intent
                          |
                  Risk / OMS interface
                          |
                 Exchange / fill model
                          |
                  Portfolio accounting
                          |
                  Metrics + report
```

## Minimum viable product (MVP)

- Event queue for market data, timers, orders, fills and corporate actions.
- Limit and market orders with configurable latency and partial-fill behavior.
- Commissions, bid/ask spread and slippage assumptions.
- Portfolio cash, positions, realized/unrealized P&L and transaction ledger.
- Performance report and walk-forward evaluation with train/test separation.

### Explicit non-goals

- Using future bars or revised data before publication time.
- Assuming full fills at the candle close for all strategies.
- Optimizing strategies until they overfit and presenting in-sample results as evidence.

## Services and responsibilities

- `data-loader`: point-in-time historical datasets and adjustments.
- `clock/event-bus`: deterministic timestamp + tie-break ordering.
- `strategy`: pure callbacks for events and signals.
- `exchange-sim`: fill/latency/slippage policies.
- `portfolio/report`: accounting, metrics, reproducibility manifests.

## Domain model (starting point)

```text
MarketEvent(ts, symbol, event_type, payload)
SimOrder(id, symbol, side, type, qty, limit_px, submitted_at)
SimFill(order_id, qty, px, fee, ts)
PortfolioSnapshot(ts, cash, holdings, equity)
BacktestRun(id, config_hash, data_version, seed, metrics)
```

Adjust schemas as requirements sharpen. Record timestamps in UTC with venue/event time versus local receive time clearly distinguished, and version decisions/rules where possible.

## API or protocol sketch

These are **illustrative contracts**, not working endpoints.

```text
python -m backtester run   --strategy examples/momentum.py   --data data/xyz_2024.parquet   --slippage-bps 3 --fee-bps 1 --seed 42

# Strategy contract
class Strategy:
    def on_market_event(self, event, context): ...
    def on_fill(self, fill, context): ...
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

1. Week 1–2: event clock, reproducible data loader and accounting.
2. Week 3–4: OMS interface, exchange simulation, fees/slippage.
3. Week 5–6: strategy examples, walk-forward evaluation and reports.
4. Week 7+: paper-trading adapter parity and scenario stress tests.

## Testing strategy

- **Unit:** validation, state transitions, numeric boundaries and all documented invariants.
- **Integration:** API, state store, message transport and simulator under retries/restarts.
- **Scenario:** missing data, clock skew, stale events, connection loss, duplicate delivery and rejected writes.
- **Replay:** fixed fixtures and seeds produce reproducible outputs; pin versions of dependencies and datasets.
- **Performance:** report realistic load shape, machine details, p50/p99 where relevant, errors and memory usage (do not report fabricated metrics).

## Definition of done

- Same dataset/configuration/seed generates identical trades and reports.
- No feature can read data timestamped later than simulated time.
- Cash and position balances reconcile to all fills and fees.
- Every report discloses assumptions, date ranges, data gaps and out-of-sample split.

## Metrics to expose

- Sharpe/Sortino, max drawdown and turnover
- Execution shortfall and fees
- Out-of-sample performance
- Run throughput measured as events per second

## Stretch goals

- Limit-order queue models and intrabar uncertainty.
- Parameter sweep with proper multiple-testing controls.
- Live/paper adapter sharing the same strategy interface.

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
