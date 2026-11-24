# Event-Driven Backtesting Engine

> Simulate strategies with realistic fills, fees, slippage, and deterministic replay.

**Status:** MVP implemented and tested. Simulation only.  
**Stack:** Python 3.12, standard library, PyArrow (Parquet input only), pytest, ruff  
**Original build specification:** [docs/blueprint.md](docs/blueprint.md)

The strategy sees only data that has been published by the simulated time. Orders pay
latency, spread, slippage and fees, and can fill partially. Every run can be reproduced
byte for byte from its manifest.

Nothing here can place a real order, and no strategy profitability is implied. The demo
and test data are invented by a seeded random walk; they are not market data.

## Quickstart

Requires Python 3.12+.

```bash
make setup              # create .venv, install the package and dev dependencies
make demo               # generate data, backtest, replay from the manifest, walk-forward
make failure-demo       # stale data, skewed clocks, duplicates, failed writes
```

```bash
make lint               # ruff check + format check
make test               # unit tests
make integration-test   # CLI, replay, walk-forward and failure-scenario tests
make report             # full run with pass/fail evidence in reports/
make bench              # throughput on this machine
```

The command from the blueprint works as written once you have a data file:

```bash
python -m backtester gen-data --out data/demo
python -m backtester run --strategy examples/momentum.py --data data/demo/bars.parquet \
    --corporate-actions data/demo/corporate_actions.csv --slippage-bps 3 --fee-bps 1 --seed 42
```

```text
SIMULATED RESULTS. Hypothetical trades against historical or synthetic data; no real orders were placed and no profitability is implied.

  run id            175f82a6bf4fdfbc
  strategy          Momentum {}
  period            2023-01-02 to 2024-12-27
  final equity      1,243,951.47  (24.40%)
  sharpe / sortino  1.10 / 1.66
  max drawdown      9.48%
  turnover          21.27x mean equity
  fees              2,438.77
  shortfall         40,172.03  (16.48 bps)
  orders            71 sent, 71 filled, 0 rejected, 0 canceled
  data gaps         1
  reconciliation    cash and positions match the ledger and fills
  config hash       sha256:55d122a15fbc454fbf8716de2d4f8cbd84c6b82dff5136b65a1f481ec6304b70
  data version      sha256:88fa5983895b51472a0da3e4d4470e86158f4026cb848eb508266cdd24a5c73d
  result hash       sha256:a03ab45755c3c4e440543b46474e2c059dece1e4f36ed2630e64fa8304bde156

  report     runs/175f82a6bf4fdfbc/report.md
  verify     python -m backtester verify runs/175f82a6bf4fdfbc
```

The return is a property of the invented prices, which are generated with an upward
drift, and of nothing else. This run has no out-of-sample split and unlimited liquidity,
and its report says so.

The run directory holds the report, the orders, fills, ledger and equity curve as CSV,
and a manifest. `python -m backtester verify runs/<run id>` re-runs it and compares
everything.

## How it works

```text
CSV / Parquet -> loader: validate, order, de-duplicate, hash
                    |
            event queue (ts, priority, seq) <----------------------+
              |                    |                               |
        at venue time      at availability time                    |
              v                    v                               |
      exchange simulator        strategy -> order intent           |
              |                              |                     |
              |                   risk gate + order manager        |
              |                              |  (after latency)    |
              +--- fills, cancels -----------+---------------------+
                    |
          portfolio + ledger -> reconciliation -> metrics -> report + manifest
```

- **Two clocks per event.** Each bar or quote has the time it happened at the venue and
  the time it became available. The exchange acts on the first, the strategy on the
  second. The loader refuses data that claims to be published before it happened.
- **One ordering.** Events are processed by `(timestamp, priority, insertion order)`.
  At the same instant: corporate actions, then the exchange, then arriving orders, then
  fills, then the strategy, then timers.
- **Pessimistic fills.** A bar can fill only orders that reached the exchange before it
  opened. Market orders fill at the next open. Limit orders fill at their limit only if
  the bar trades through it. Costs always round against the trader.
- **Exact money.** Prices, cash, fees and P&L are integers in units of 10^-4. The
  accounting identity is checked to the unit at the end of every run.
- **Seeded randomness.** Latency jitter, touch fills and `context.rng` each have their
  own stream derived from the run seed. Nothing else is random.

Details and the invariant-to-test map are in [docs/architecture.md](docs/architecture.md).
The hardest trade-off, what a bar is allowed to fill, is written up in
[ADR 0001](docs/decisions/0001-bar-fill-eligibility.md). Data formats, flags, how to read
a report and what to do when the engine refuses are in [docs/runbook.md](docs/runbook.md).

## Strategy contract

```python
from backtester import Strategy

class Example(Strategy):
    def on_market_event(self, event, context):
        bars = context.history(event.symbol, 20)       # only bars already published
        if len(bars) == 20 and not context.position(event.symbol):
            context.submit_order(event.symbol, "buy", 10)

    def on_fill(self, fill, context): ...
```

Also available: `on_start`, `on_order_update`, `on_timer`, `on_corporate_action`,
`on_end`. The context offers `now`, `cash`, `equity`, `position`, `mark`, `history`,
`last_quote`, `open_orders`, `submit_order` (market or limit, GTC or IOC), `cancel_order`,
`set_timer` and a seeded `rng`. It has no access to the dataset. Three examples are in
[examples/](examples/): [momentum](examples/momentum.py),
[mean reversion with limit orders](examples/mean_reversion.py) and
[buy and hold](examples/buy_and_hold.py).

## Walk-forward

```bash
python -m backtester walk-forward --strategy examples/momentum.py \
    --data data/demo/bars.parquet --corporate-actions data/demo/corporate_actions.csv \
    --grid lookback=10,20,40,80 --grid threshold_bps=0,200 --train-days 365 --test-days 91
```

For each fold, every parameter combination runs on the training window, the best one
runs once on the following test window, and the report shows the in-sample and
out-of-sample figures side by side. Training runs are given a dataset that ends before
the test window. The report states how many backtests were run and that no correction
for multiple testing is applied.

## Definition of done

| Requirement | Evidence |
|---|---|
| Same dataset, configuration and seed generate identical trades and reports | [tests/integration/test_determinism.py](tests/integration/test_determinism.py): repeat runs compare every artifact; two separate processes with different `PYTHONHASHSEED` write byte-identical run directories; three golden cases replay against committed hashes. `verify` is tested against edited artifacts, changed data and a changed strategy in [tests/integration/test_cli.py](tests/integration/test_cli.py). |
| No feature can read data timestamped later than simulated time | [tests/scenarios/test_no_lookahead.py](tests/scenarios/test_no_lookahead.py): deleting or rewriting all data after a cutoff leaves everything before it unchanged (what the strategy was shown, orders, fills, ledger, equity), for two strategies at several cutoffs with every friction on. Delayed publication, split restatement and walk-forward isolation are tested separately. |
| Cash and position balances reconcile to all fills and fees | [tests/scenarios/test_definition_of_done.py](tests/scenarios/test_definition_of_done.py): 16 runs of a strategy that trades at random (both sides, both order types, cancels, shorts, partial fills, through a split and a dividend) reconcile exactly; a forged fill is detected. The engine runs the same check at the end of every backtest. |
| Every report discloses assumptions, date ranges, data gaps and the out-of-sample split | Same file: the run report and the walk-forward report are checked for each disclosure. A run without a split says its figures are in-sample. Undefined metrics print as `n/a`. |

Latest local run, 2026-11-24: 362 tests passing (241 unit, 55 integration, 66 scenario)
on CPython 3.12.14, macOS arm64. `make report` regenerates the evidence.

## Metrics

| Blueprint metric | Where |
|---|---|
| Sharpe, Sortino, max drawdown, turnover | Report and `metrics.json`; from daily equity, annualised with a stated period count. |
| Execution shortfall and fees | Shortfall is measured against the price when the strategy decided, in currency and basis points. |
| Out-of-sample performance | `--oos-start` on a single run; per fold and chained in walk-forward. |
| Run throughput in events per second | Printed after every run and written to `runtime.json`, which is excluded from all hashes. |

Benchmark, one machine (Apple silicon, 10 logical CPUs, macOS 26.5.1, CPython 3.12.14),
487,500 synthetic one-minute bars, median of five runs, including reconciliation,
metrics and hashing the output:

| Workload | Events | Fills | Whole run | Events/s |
|---|---|---|---|---|
| Strategy that never trades | 975,000 | 0 | 0.63 s | 1,542,469 |
| Momentum with fees, slippage, latency and a 5% participation cap | 1,110,578 | 120,722 | 2.93 s | 379,573 |

Peak memory was 762 MB across all ten runs in one process. Method, the event-loop-only
figures and caveats are in the [runbook](docs/runbook.md#benchmark). These numbers say
nothing about other machines or other strategies.

## Implemented vs. Planned

### Implemented

| Area | Where |
|---|---|
| Event queue for market data, timers, order arrivals, executions and corporate actions, with deterministic tie-breaking | [src/backtester/eventbus.py](src/backtester/eventbus.py), [src/backtester/engine.py](src/backtester/engine.py), [tests/unit/test_engine.py](tests/unit/test_engine.py) |
| Market and limit orders, GTC and IOC, cancels, configurable latency with seeded jitter | [src/backtester/exchange.py](src/backtester/exchange.py), [tests/unit/test_exchange.py](tests/unit/test_exchange.py) |
| Partial fills from a volume participation cap; seeded touch-fill probability | same |
| Commissions (bps and per unit), assumed spread for bars, quoted spread for quotes, slippage | [src/backtester/fees.py](src/backtester/fees.py), same |
| Pre-trade risk gate with reason codes; order state machine | [src/backtester/risk.py](src/backtester/risk.py), [src/backtester/orders.py](src/backtester/orders.py), [tests/unit/test_orders_and_risk.py](tests/unit/test_orders_and_risk.py) |
| Cash, positions, realized and unrealized P&L, transaction ledger, reconciliation | [src/backtester/portfolio.py](src/backtester/portfolio.py), [tests/unit/test_portfolio.py](tests/unit/test_portfolio.py) |
| Splits (with cash in lieu and history restatement) and cash dividends | [src/backtester/engine.py](src/backtester/engine.py), [tests/unit/test_engine.py](tests/unit/test_engine.py) |
| CSV and Parquet loading with validation, de-duplication, gap detection and a content hash | [src/backtester/data.py](src/backtester/data.py), [tests/unit/test_data.py](tests/unit/test_data.py) |
| Performance metrics and a report that states its assumptions | [src/backtester/metrics.py](src/backtester/metrics.py), [src/backtester/report.py](src/backtester/report.py) |
| Walk-forward evaluation with train/test separation | [src/backtester/walkforward.py](src/backtester/walkforward.py), [tests/integration/test_walkforward.py](tests/integration/test_walkforward.py) |
| Reproducibility manifest and `verify` | [src/backtester/report.py](src/backtester/report.py), [src/backtester/cli.py](src/backtester/cli.py) |
| Golden replay fixture and its regeneration script | [tests/fixtures/golden/](tests/fixtures/golden/), [scripts/update_golden.py](scripts/update_golden.py) |
| Failure scenarios: missing data, clock skew, stale data, duplicates, conflicting revisions, rejected writes | [tests/scenarios/test_data_failures.py](tests/scenarios/test_data_failures.py), [examples/failure_demo.py](examples/failure_demo.py) |
| Terminal walkthrough, benchmark script, CI workflow, Dockerfile, compose file | [src/backtester/demo.py](src/backtester/demo.py), [scripts/benchmark.py](scripts/benchmark.py), [.github/workflows/ci.yml](.github/workflows/ci.yml) |

### Differences from the original blueprint

- **No pandas, Polars, DuckDB or Rust.** The engine is plain Python over integers.
  PyArrow is used only to read and write Parquet.
- **One process, not five services.** The five responsibilities are separate modules.
- **The domain model has more fields.** `MarketEvent` carries both `ts_event` and `ts`;
  `SimOrder` adds time in force, a tag and the decision price; `SimFill` adds its own id,
  symbol, side and maker/taker flag.
- **Integration tests cover the command line, files and replay.** The blueprint lists an
  API, a state store and a message transport; this design has none of them.
- **No latency percentiles.** It is a batch program, so the benchmark reports throughput,
  spread across repeats and peak memory.
- **No stop orders.** They cannot be simulated honestly from bars (see the ADR).

### Planned / not done

- Paper or live adapter behind the same `Strategy` and `Context` interface. `Context` is
  an abstract class, but no second implementation exists.
- Limit-order queue models and intrabar uncertainty beyond the touch-fill probability.
- Multiple-testing control for parameter sweeps (for example a deflated Sharpe ratio).
  Today the report only states how many combinations were tried.
- Market impact, borrow costs, margin interest, taxes, multiple currencies.
- Exchange holiday and session calendars. Gap detection is a weekday heuristic.
- Point-in-time handling of revised data. Conflicting versions of a bar are refused.
- Streaming or out-of-core data. Everything is held in memory.
- A screen capture. The walkthrough is `make demo` in a terminal.

## Not verified

- The CI workflow has never run; this directory is not a Git repository yet.
- The test suite has only been run on macOS arm64 with CPython 3.12.14. The demo was
  also run once in the Docker image (Linux arm64, Python 3.12.16) and produced the same
  run id and result hash. No x86, Windows or Python 3.13 run has been done.
- No real market data has been loaded. Every test and demo uses synthetic prices, so
  vendor file quirks are untested.
- The largest run so far is the benchmark's 487,500 bars.
- Quote-driven fills have unit tests and one small end-to-end run; they are less
  exercised than bar fills.
- PyArrow is declared as `>=17,<27` but only 26.0.1 has been tried.
- The `--user` advice for Docker on Linux in the runbook.

## Safety boundaries

- Simulated only. There is no broker, exchange or network code in the package, and the
  compose file runs the container with networking disabled.
- No credentials are needed or read.
- Money arithmetic is integer fixed-point throughout the execution path.
- Every report and terminal summary is labelled as simulated. Figures in this README
  come from the runs described next to them.

## Repository layout

```text
src/backtester/     engine, exchange simulator, portfolio, loader, reports, CLI
examples/           three strategies and the failure demo
scripts/            benchmark, golden fixture regeneration
tests/unit/         pure logic
tests/integration/  CLI, determinism and replay, walk-forward
tests/scenarios/    look-ahead, data failures, definition of done
tests/fixtures/     committed golden replay data (synthetic)
docs/               architecture, ADR, runbook, original blueprint
```

## License

No license has been chosen. Choose one before publishing; MIT and Apache-2.0 are common.
