# Market Data Ingestion Platform

> A scalable pipeline for normalized tick feeds, durable time-series data, and live subscriptions.

**Difficulty:** Advanced  
**Estimated build window:** 6–9 weeks (illustrative, solo development)  
**Suggested stack:** Rust/Go, Kafka/Redpanda, ClickHouse, Parquet/S3-compatible storage, Redis, WebSockets, Prometheus  
**Status:** Project blueprint — not yet implemented

## Problem and outcome

Ingest multiple simulated or licensed market feeds, normalize instrument/event schemas, make them queryable, and deliver data with bounded lag. Focus on backpressure, ordering, timestamp provenance and operational transparency.

## System architecture

```text
Feed A    Feed B    Replay source
   |         |           |
   +---- feed adapters --+
             |
        Normalization
             |
      Kafka / Redpanda
          /      \
 ClickHouse     Live WebSocket
      |
Parquet archive / historical query API
```

## Minimum viable product (MVP)

- At least two schema-compatible feed adapters (one can be replay).
- Canonical trade, quote, and optional depth messages.
- Durable topic partitioning by instrument and sequence metadata.
- ClickHouse ingest plus historical query endpoint.
- WebSocket subscriptions with freshness, dropped-event and gap indicators.

### Explicit non-goals

- Redistributing proprietary market data without a license.
- Conflating provider event time with local receive time.
- Guaranteeing universal total ordering across independent venues.

## Services and responsibilities

- `ingestors`: provider-specific parsers and reconnect behavior.
- `normalizer`: instrument IDs, decimal ticks, event semantics.
- `stream`: partitioned retention, consumers and backpressure.
- `storage`: ClickHouse/Parquet historical facts.
- `gateway`: query, subscriptions, entitlements and metrics.

## Domain model (starting point)

```text
Instrument(id, venue, native_symbol, tick_size, currency)
MarketTrade(instrument_id, trade_id, price_ticks, qty, event_ts, recv_ts)
MarketQuote(instrument_id, bid, ask, sizes, seq, event_ts)
FeedOffset(provider, partition, seq, resume_token)
IngestHealth(provider, lag_ms, gaps, reconnects)
```

Adjust schemas as requirements sharpen. Record timestamps in UTC with venue/event time versus local receive time clearly distinguished, and version decisions/rules where possible.

## API or protocol sketch

These are **illustrative contracts**, not working endpoints.

```text
GET /v1/instruments?search=XYZ
GET /v1/trades?instrument_id=...&from=...&to=...
WS  /v1/stream?instrument_id=...

{"type":"trade","instrument_id":"venueA:XYZ",
 "price_ticks":12345,"quantity":100,
 "event_time":"2026-01-01T10:00:00Z","receive_time":"..."}
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

1. Week 1: schemas, provenance model, fixture feeds and replay.
2. Week 2–3: ingestion, Kafka topics and at-least-once normalization.
3. Week 4–5: ClickHouse tables, historical queries and subscriptions.
4. Week 6+: soak tests, reconnects, lag dashboards and recovery playbook.

## Testing strategy

- **Unit:** validation, state transitions, numeric boundaries and all documented invariants.
- **Integration:** API, state store, message transport and simulator under retries/restarts.
- **Scenario:** missing data, clock skew, stale events, connection loss, duplicate delivery and rejected writes.
- **Replay:** fixed fixtures and seeds produce reproducible outputs; pin versions of dependencies and datasets.
- **Performance:** report realistic load shape, machine details, p50/p99 where relevant, errors and memory usage (do not report fabricated metrics).

## Definition of done

- Restart/replay does not silently duplicate canonical persisted facts.
- At-least-once behavior and deduplication keys are documented.
- Backpressure generates observable lag rather than silent drops.
- All timestamps identify their clock/source and query retention is defined.

## Metrics to expose

- Events/sec and bytes/sec
- End-to-end ingestion p50/p99 latency
- Consumer lag and replay catch-up time
- Data gaps, duplicates and reconnect frequency

## Stretch goals

- Schema registry and backward-compatible event evolution.
- Entitlement-aware market-data gateway.
- Tiered hot/cold historical storage and downsampling.

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
