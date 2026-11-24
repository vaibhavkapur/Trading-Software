# Runbook

The engine is an offline batch program. There is no service to operate; this page covers
running it, feeding it data, reading what it writes, and what to do when it refuses.

## Setup

```sh
make setup            # creates .venv and installs the package with dev dependencies
make lint test integration-test
make demo             # writes data/demo and runs/demo-*
```

Requires Python 3.12 or later. The only runtime dependency is `pyarrow`, used for Parquet
files; CSV input works without ever importing it.

## Commands

| Command | Purpose |
|---|---|
| `python -m backtester run ...` | One backtest. Prints a summary and writes a run directory. |
| `python -m backtester walk-forward ...` | Choose parameters on training windows, evaluate on unseen test windows. |
| `python -m backtester verify <run dir>` | Re-run a saved run from its manifest and compare every hash and artifact. |
| `python -m backtester gen-data --out <dir>` | Write a synthetic dataset. The prices are invented. |

`--help` on any command lists every flag. Exit codes: `0` success, `1` the operation
failed (a write error, or `verify` found a difference), `2` the input was wrong (bad
flag, unreadable or invalid data, unusable strategy file).

Flags that set assumptions, with their defaults:

| Flag | Default | Meaning |
|---|---|---|
| `--fee-bps`, `--fee-per-unit` | 0, 0 | Commission on notional and per unit, rounded up. |
| `--slippage-bps` | 0 | Adverse move on every liquidity-taking fill. |
| `--spread-bps` | 0 | Assumed full bid/ask spread for bar data; takers pay half. |
| `--latency-ms`, `--latency-jitter-ms` | 0, 0 | Delay from the strategy to the exchange, plus seeded jitter. |
| `--participation` | unlimited | Largest share of a bar's volume (or a quote's size) that may be filled. |
| `--touch-fill-prob` | 0 | Chance a limit order fills when the bar only touches its price. |
| `--allow-short`, `--allow-leverage` | off | Without them, selling more than is held or buying beyond cash is rejected. |
| `--max-data-age-s` | off | Reject orders in a symbol whose last data is older than this. |
| `--publish-delay-s` | 0 | Delay from an event to its availability, for files without a `ts_avail` column. |
| `--oos-start` | none | Report in-sample and out-of-sample separately from this time. |

The defaults are frictionless. A run with all of them left at zero is not a realistic
estimate of anything, and its report says which assumptions were used.

## Input data

Bars, CSV or Parquet:

| Column | Required | Notes |
|---|---|---|
| `symbol` | or pass `--symbol` | |
| `ts_open` | yes | Also accepted: `timestamp`, `ts`, `datetime`, `date`, `time`. |
| `ts_close` | or pass `--bar-seconds` | |
| `ts_avail` | no | When the bar became known. Defaults to the close plus `--publish-delay-s`. |
| `open`, `high`, `low`, `close` | yes | Decimal text or numbers; stored as integers at `--price-decimals` places. |
| `volume` | no | Whole units. Needed for `--participation` to mean anything. |

Quotes (`--quotes`): `symbol`, `ts_event`, `bid`, `ask`, `bid_size`, `ask_size`, and
optionally `ts_avail`. A symbol may have bars or quotes, not both.

Corporate actions (`--corporate-actions`, CSV): `ts`, `symbol`, `type`, `value`, where
`type` is `split` with a value like `2:1`, or `dividend` with a cash amount per unit.

Timestamps are ISO-8601 or, in Parquet, timestamp columns. A timestamp without a zone is
read as UTC. Prices must be as traded (unadjusted); the engine applies splits itself and
would double-count them on back-adjusted data.

The loader stops at the first invalid row. `--on-invalid drop` drops such rows instead
and the report states how many. Exact duplicate rows are always dropped and counted. Row
order in the file does not matter.

## Writing a strategy

A strategy is a Python file with one subclass of `backtester.Strategy`:

```python
from backtester import Strategy

class Example(Strategy):
    def __init__(self, lookback: int = 20):
        self.lookback = lookback

    def on_market_event(self, event, context):
        bars = context.history(event.symbol, self.lookback)
        if len(bars) == self.lookback and not context.position(event.symbol):
            context.submit_order(event.symbol, "buy", 10)

    def on_fill(self, fill, context):
        ...
```

Other callbacks: `on_start`, `on_order_update` (rejections and cancels arrive here),
`on_timer`, `on_corporate_action`, `on_end`. Constructor arguments come from
`--param name=value`.

Rules for a strategy that reproduces:

- Use `context.rng` for randomness, never the `random` module or `numpy.random`.
- Do not read the wall clock, files or the network. `context.now` is the time.
- Prices are integers in price units. `context.price("101.25")` converts.
- The context only exposes data already published. Keep it that way: do not load the
  data file yourself.

The engine hashes the strategy file into the run's `config_hash`, so editing the file
changes the run id even if the trades do not change.

## Reading a run directory

| File | Contents |
|---|---|
| `report.md` | Identifiers, data coverage and gaps, every assumption, the out-of-sample split, metrics, execution statistics, reconciliation. Start here. |
| `metrics.json` | The same numbers for machines. Undefined values are `null`, never 0. |
| `orders.csv`, `fills.csv` | Every order with its final status and reason; every fill. |
| `ledger.csv` | Every cash movement and position change, with a running cash balance. |
| `equity.csv` | Cash, equity and holdings at every instant something changed. |
| `manifest.json` | What is needed to reproduce the run, and a hash of each file above. |
| `runtime.json` | Wall-clock timings. Not reproducible, not hashed. |

Things to check before believing a result:

- **Out-of-sample split.** Without one, every figure is in-sample.
- **Liquidity.** "Unlimited" means order size was never constrained.
- **Data gaps and dropped rows.** Orders do not fill during a gap and positions keep
  their last price.
- **Rejected orders.** A high count usually means the strategy is sizing against cash it
  does not have, or trading before data exists.
- **Minimum cash.** Negative means a fill at a worse price than expected overdrew the
  account.
- **Sharpe or Sortino shown as n/a.** There were too few daily observations, or no
  variation, to define them.

## Reproducing a run

```sh
python -m backtester verify runs/<run id>
```

This reloads the recorded input files and strategy, re-runs with the recorded
configuration, and compares the data version, config hash, result hash and each artifact
both as regenerated and as found on disk. Recorded paths are relative to where the run
was started; use `--base-dir` if you are somewhere else.

| `verify` reports | Meaning |
|---|---|
| `FAIL data version` | An input file's content has changed since the run. |
| `FAIL config hash` | The strategy file or the engine version has changed. |
| `FAIL result hash` | The trades differ. Expected after either of the above; a bug if alone. |
| `FAIL <file> (file on disk)` | That artifact was edited or deleted after the run. |

## Failure handling

`make failure-demo` shows each of these live.

| Situation | Behaviour |
|---|---|
| A symbol stops publishing | Its orders stay open and unfilled; its position keeps the last price; the gap is listed in the report. With `--max-data-age-s`, new orders in it are rejected as `STALE_MARKET_DATA`. |
| A row is published before it happened (clock skew, time zone error) | The load is refused and the row identified. |
| Duplicate or out-of-order rows | Exact duplicates are dropped and counted; order does not matter; the data version is the same as for the clean file. |
| The same bar twice with different values | Refused. Revised data is not supported. |
| A malformed file | Refused with the file, the row or column, and the reason. |
| The output directory cannot be written | Exit code 1, nothing left behind. An existing run at that path is untouched unless the new one is complete. |
| A strategy raises | The run stops with the traceback and the simulated time at which it happened. No artifacts are written. |
| Books do not reconcile | The run stops with `ReconciliationError`. This is an engine bug; report it with the manifest. |

## Benchmark

`make bench` runs `scripts/benchmark.py`, which documents its own method. In short: one
process, one thread, synthetic one-minute bars generated in memory, each workload
repeated five times, each bar counted as two events (venue time and availability time).

One measurement, taken on 2026-11-24:

| | |
|---|---|
| Machine | Apple silicon (arm64), 10 logical CPUs, macOS 26.5.1 |
| Python | CPython 3.12.14 |
| Dataset | 487,500 one-minute bars: 5 symbols over 250 days |

| Workload | Events | Fills | Event loop, events/s (best / median / worst) | Whole run, median |
|---|---|---|---|---|
| Idle strategy, no orders | 975,000 | 0 | 3,093,095 / 2,954,810 / 1,959,857 | 0.63 s, 1,542,469 events/s |
| Momentum, lookback 60, fees, slippage, latency, 5% participation | 1,110,578 | 120,722 | 753,961 / 641,487 / 569,066 | 2.93 s, 379,573 events/s |

Peak resident memory was 762 MB for the dataset plus all ten runs' results.

"Whole run" includes reconciliation, metrics and hashing every row of output, and is the
number to plan with. These figures are from a single machine and say nothing about
others. The second workload is slower because it trades heavily, about one fill for
every four bars, and each fill passes through the order manager, portfolio, ledger and
output files. A strategy that does more work per bar will be slower still.

Loading files is not part of the benchmark. In one check on the same machine, loading
97,500 bars took 0.63 s from CSV and 0.28 s from Parquet, against 1.05 s for the momentum
run over them.

## Maintenance

- **Changing fill rules or accounting.** Bump `FILL_MODEL_VERSION` in
  `src/backtester/_version.py`, run `python scripts/update_golden.py`, and review the diff
  of `tests/fixtures/golden/cases.json`. The golden replay test fails until this is done.
- **Changing an example strategy** changes the golden expectations for that case the
  same way.
- **Test report.** `make report` writes `reports/test-report.txt` and
  `reports/junit.xml`.

## Docker

```sh
docker compose run --rm backtester          # the demo; reports appear in ./runs
```

The container runs without network access and as a non-root user. On Linux, add
`--user "$(id -u):$(id -g)"` so it can write to the mounted `./data` and `./runs`.
