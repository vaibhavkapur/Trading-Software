# Runbook

Everything here runs on simulated data. The service has no market or broker connectivity.

## Run

```bash
make setup          # once
make run            # http://127.0.0.1:8009, dashboard at /, OpenAPI at /docs
```

`make run` loads `examples/data` into an in-memory database. To keep data across restarts
and load your own files:

```bash
PYTHONPATH=src .venv/bin/python -m tca import path/to/dir --db data/tca.sqlite3
TCA_DB_PATH=data/tca.sqlite3 PYTHONPATH=src .venv/bin/python -m tca serve
```

`import` looks for `orders`, `child_orders`, `fills`, `quotes` and `bars` with a `.csv` or
`.parquet` extension (Parquet needs `pip install -e ".[parquet]"`). Add `--strict` to exit
non-zero if any row is rejected.

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `TCA_HOST` | `127.0.0.1` | There is no authentication; do not bind to a public interface |
| `TCA_PORT` | `8009` | |
| `TCA_DB_PATH` | `:memory:` | SQLite file path to persist imports |
| `TCA_MAX_QUOTE_AGE_MS` | `5000` | Quotes older than this are stale |
| `TCA_MAX_CLOCK_SKEW_MS` | `250` | Largest tolerated gap between a quote's event and receive time |
| `TCA_QUOTE_CLOCK` | `event` | `event` or `receive` |

The last three are methodology parameters. Changing any of them changes the methodology id
on every output, so reports made before and after are visibly not comparable.

## Input formats

Timestamps are ISO-8601 with a UTC offset (`2026-03-02T15:00:00Z`). Prices and fees are
decimal strings. Quantities are whole numbers.

| Dataset | Columns | Key |
|---|---|---|
| `orders` | `order_id, symbol, side (buy/sell), qty, decision_ts, arrival_ts, broker, strategy` | `order_id` |
| `child_orders` | `child_id, order_id, venue, qty, sent_ts` | `child_id` |
| `fills` | `fill_id, order_id, venue, qty, price, fee, ts`, optional `child_id` | `fill_id` |
| `quotes` | `symbol, venue, bid, ask, ts`, optional `recv_ts` | `symbol, venue, ts` |
| `bars` | `symbol, ts, interval_s, vwap, volume` | `symbol, ts` |

A negative `fee` is a rebate. `child_orders` and `bars` are optional: without child orders
there is no per-venue fill rate, and without bars there is no interval VWAP.

Over HTTP, post a CSV body, or JSON `{"rows": [...]}` with `content-type: application/json`,
to `/v1/imports/{orders|child-orders|fills|quotes|bars}`.

## Import results

Every import returns `rows`, `accepted`, `duplicates`, `rejected` and the first 100 rejects.

| Outcome | Meaning | Action |
|---|---|---|
| `duplicates > 0` | Rows already stored, byte-for-byte equivalent | None. Re-delivery is safe |
| `CONFLICT` | A different row under an existing key | The stored row was kept. If the new one is the correction, start a new database; restatements are not applied in place |
| `BAD_TIMESTAMP` | Unparseable, or no UTC offset | Fix the export; the service will not guess a timezone |
| `BAD_NUMBER`, `NOT_WHOLE`, `OUT_OF_RANGE`, `NON_POSITIVE`, `BAD_SIDE`, `MISSING_FIELD` | Field validation | Fix the row and resend the file; accepted rows will count as duplicates |
| `DECISION_AFTER_ARRIVAL` | `decision_ts` later than `arrival_ts` | Check which clock stamped each field |
| `CROSSED_QUOTE` | One venue's bid above its own ask | Bad tick; dropped |
| `BAR_MISALIGNED`, `BAR_INTERVAL_MISMATCH` | Bar start not on an interval boundary, or a second interval for a symbol | Use one bar size per symbol |
| HTTP 400 | Missing or unknown columns, empty or undecodable payload | Nothing was stored. Fix the header |
| HTTP 413 | Body over 64 MiB | Split the file |

`GET /v1/datasets` shows row counts, versions, recent imports and `orphan_fills` (fills
whose `order_id` matches no order). Orphans usually mean the orders file has not been
loaded yet, or covers a different period.

## Reading order flags

The per-order view (`GET /v1/tca/orders/{id}` or a click in the dashboard) lists flags
with a plain-language note for each. The full table is in
[methodology.md](methodology.md#flags). The common ones:

| Flag | Likely cause | What to do |
|---|---|---|
| `ARRIVAL_QUOTE_MISSING` | Quote capture started after the order, or wrong symbol mapping | Import earlier quotes. The order stays without an arrival benchmark until then |
| `ARRIVAL_QUOTE_STALE` | Feed outage or an illiquid symbol | Import the missing quotes if they exist. For illiquid names consider a larger `TCA_MAX_QUOTE_AGE_MS`, knowing it changes the methodology id |
| `*_CLOCK_SKEW` | Venue or capture clock drift | Check time sync on the capture host; compare results under `TCA_QUOTE_CLOCK=receive` |
| `*_QUOTE_CROSSED` | Venues out of sync with each other, often also a clock problem | Inspect the quotes in the overlay chart |
| `OVERFILLED`, `FILL_BEFORE_ARRIVAL` | Duplicate fills under different ids, fills attached to the wrong order, or order and fill clocks disagree | Fix the source data. The order is excluded from trusted aggregates |
| `VWAP_BARS_PARTIAL`, `VWAP_BARS_COARSE` | Missing bars, or an order shorter than one bar | Import the bars, or use smaller bars |

A low `trusted_share` on a summary row means the cost figure next to it covers only part
of that group. Compare with `include_untrusted=true` to see how sensitive it is.

## Reports

```bash
PYTHONPATH=src .venv/bin/python -m tca report --data examples/data --out reports/latest
PYTHONPATH=src .venv/bin/python -m tca report --db data/tca.sqlite3 --out reports/latest \
    --benchmark interval_vwap --include-untrusted --bucket-minutes 15
```

A bundle is `orders.csv`, `fills.csv`, `benchmarks.csv`, seven `summary_by_*.csv` files
and `manifest.json`. To check a bundle someone hands you:

```bash
.venv/bin/python scripts/reconcile.py --data examples/data --report examples/report
```

This verifies the file hashes in the manifest and recomputes arrival shortfall from the raw
inputs with exact fractions, without importing the `tca` package. Exit status 1 means a
file was altered or a figure does not reconcile.

Two bundles with the same `report_id` have the same methodology, dataset version, options
and tool version, and should be byte-identical.

## Regenerating committed artefacts

| Command | Rewrites | When |
|---|---|---|
| `make fixtures` | `examples/data/` | After changing the simulator |
| `make golden` | `examples/report/` | After any intended change to calculations or output format |

`tests/scenarios/test_replay.py` fails until both match the code. Review the diff of
`examples/report/` before committing: it is the record of what the change did to the
numbers.

## Metrics

`GET /metrics` (Prometheus text format).

| Metric | Meaning |
|---|---|
| `tca_shortfall_bps{benchmark}` | Slippage over all trusted orders. `NaN` when nothing can be costed |
| `tca_cost_bps{strategy, component="fees"\|"spread"}` | Fees and spread paid by strategy, trusted orders |
| `tca_fill_rate` | Filled over ordered quantity, all orders |
| `tca_time_to_fill_seconds{fill="first"\|"last"}` | Mean time from arrival |
| `tca_trusted_benchmark_share{benchmark}` | Share of filled orders with a trusted benchmark |
| `tca_orders{state}`, `tca_orphan_fills` | Order counts and unmatched fills |
| `tca_import_rows_total{dataset, outcome}` | Accepted, duplicate and rejected rows |
| `tca_analysis_seconds` | Time to recompute after data changed |

The gauges refresh when the analysis is recomputed, which happens on the first read after
an import. Useful alerts: a falling `tca_trusted_benchmark_share`, a non-zero
`tca_orphan_fills` that persists, and any growth in rejected rows.
