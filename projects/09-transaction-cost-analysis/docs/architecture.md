# Architecture

One Python process. Data comes in as files or HTTP payloads, is validated and stored in
SQLite, and every result is recomputed from the stored data on demand. There are no
outbound connections and no credentials.

```text
 orders / child orders / fills            quotes / bars
 (CSV, Parquet, JSON rows)                (CSV, Parquet, JSON rows)
              |                                  |
              +------------- importer -----------+     validate each row, reject with a reason
                                |
                              store                    SQLite, idempotent on natural keys,
                                |                      content-hash dataset versions
                     immutable Dataset snapshot
                                |
              +-----------------+------------------+
              |                                    |
      QuoteBook / BarBook                    Methodology          thresholds + quote clock,
      (as-of lookups, never look ahead)            |              hashed into the version id
              |                                    |
              +----------- attribution ------------+             per-order signed costs,
                                |                                flags and notes
                           aggregate                             ratio-of-sums by group,
                                |                                trusted orders only
             +------------------+-------------------+
             |                  |                   |
        report bundle        HTTP API           Prometheus
        (CSV + manifest)   + dashboard           /metrics
```

## Modules

| Module | Responsibility | Blueprint service |
|---|---|---|
| `tca.importer` | Parse CSV, Parquet and JSON rows into typed records; reject bad rows individually with a code | importer |
| `tca.store` | Persist records; duplicates are no-ops, conflicting restatements are rejected; compute dataset versions | importer |
| `tca.benchmark` | `Methodology`, point-in-time consolidated quotes, interval VWAP, quality status | benchmark |
| `tca.attribution` | Signed costs per order, spread/drift split, flags, confidence notes | attribution |
| `tca.aggregate` | Group summaries by broker, symbol, strategy, side, venue and arrival-time bucket | attribution |
| `tca.render`, `tca.report` | Fixed-point serialisation, CSV exports, the versioned report bundle | report-api |
| `tca.service`, `tca.api` | Cached analysis, filters, HTTP endpoints | report-api |
| `tca.web` | Single-page dashboard: summary chart and table, order list, drill-down with benchmark overlay | dashboard |
| `tca.simulator` | Deterministic fixture generator with planted data-quality damage | fixture source |
| `tca.metrics` | Prometheus gauges and counters | metrics |

`benchmark`, `attribution` and `aggregate` are pure: they take records and a methodology and
return values. They do no I/O and read no clock, which is what makes results reproducible
and lets the unit tests build cases by hand.

## Data flow and consistency

- **Import.** Each row is validated on its own. A bad row is rejected with a code
  (`BAD_TIMESTAMP`, `CROSSED_QUOTE`, ...) and the rest of the batch is stored. A payload
  with missing or unknown columns is refused whole, because a misspelt column would
  otherwise load silently as empty.
- **Idempotency.** Natural keys: `order_id`, `child_id`, `fill_id`, `(symbol, venue, ts)`
  for quotes, `(symbol, ts)` for bars. Re-delivering a row changes nothing. A different row
  under an existing key is rejected as `CONFLICT`; history is never overwritten.
- **Ordering.** Datasets can arrive in any order and any batching. Fills whose order has
  not arrived yet are counted as orphans and attach when the order is imported.
- **Versions.** Each dataset's version is a SHA-256 over its canonical rows in key order,
  so it depends on content only. The combined dataset version hashes the five together.
- **Analysis cache.** The store keeps a revision counter that only moves when a row is
  actually written. The service recomputes the analysis when the revision changes and
  otherwise serves the cached one, so all reads between imports see one consistent
  snapshot.
- **Restart.** With `TCA_DB_PATH` set, the data survives a restart and the first read
  recomputes the same results. The default is in-memory.

## Invariants and where they are tested

| Invariant | Test |
|---|---|
| Cost is positive exactly when the fill is worse than the benchmark, for both sides | `tests/unit/test_attribution.py`, `tests/scenarios/test_definition_of_done.py` section 1 |
| No benchmark ever uses a quote from after the instant it describes | `tests/unit/test_benchmark.py`, definition-of-done section 2 |
| spread + drift = arrival shortfall; shortfall + fees = all-in; in groups spread + drift + unsplit = slippage | `tests/unit/test_attribution.py`, `tests/unit/test_aggregate.py` |
| Untrusted benchmarks never enter an aggregate unless asked for; missing ones never do | `tests/unit/test_aggregate.py`, `tests/scenarios/test_data_quality.py` |
| Every output states the methodology id and dataset version | definition-of-done section 3 |
| Same data in any order or batching gives the same version and byte-identical report | `tests/scenarios/test_data_quality.py`, `tests/scenarios/test_replay.py` |
| Re-delivery is a no-op; conflicting restatements are rejected | `tests/integration/test_store.py`, `tests/scenarios/test_data_quality.py` |
| Results reconcile with an independent exact-fraction calculation | definition-of-done section 4, `scripts/reconcile.py` |

## Boundaries

- **No authentication.** The API accepts imports from anyone who can reach it. It binds to
  `127.0.0.1` by default and the compose file publishes on loopback only. Put real
  authentication in front before exposing it anywhere.
- **Untrusted input.** Imports are size-limited (64 MiB per request) and every field is
  validated. The dashboard writes all data into the page through DOM text nodes, never as
  HTML, so order ids or venue names containing markup are displayed, not executed.
- **Money.** Prices, fees and costs are `Decimal` from parsing to output and are stored as
  canonical text. Quantities are whole shares. The only floats are the Prometheus gauges.
- **No market connectivity.** The service reads what it is given. The simulator produces
  files; it is not a feed.

## Scaling limits

The analysis holds the whole dataset in memory and recomputes every order when anything
changes. That is appropriate for the fixture (about fifteen thousand quotes) and for
single-day samples. It has not been load-tested and no performance figures are claimed.
The natural next steps for larger data are incremental analysis per symbol and a columnar
store such as DuckDB; see "Planned" in the README.
