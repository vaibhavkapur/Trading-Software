# Market Data Ingestion Platform

Ingests market data feeds, normalizes them to one schema, publishes them to a
partitioned log (Redpanda/Kafka), stores them in ClickHouse, and serves
historical queries over HTTP and live subscriptions over WebSocket.

> **Everything this project ingests is simulated.** The two "venues" are
> generators in this repository. There is no connection to any real exchange,
> broker, account or paid data feed, and no order entry of any kind.

The brief this implements is kept unchanged in [docs/SPEC.md](docs/SPEC.md).

```
simulated venues ──► ingestors ──► md.raw ──► normalizer ──► md.trades / quotes / depth / gaps
 alpha (WebSocket)                                               │                │
 beta  (TCP)                                                     ▼                ▼
 capture file (replay)                                    sink ► ClickHouse ◄── gateway ──► HTTP, WebSocket, /metrics
```

## Quick start

Requires Go 1.27+. Docker is needed only for the parts marked so.

```sh
make setup     # download modules; write .env with a random local ClickHouse password
make test      # unit + scenario tests with the race detector (no Docker, ~15 s)
make demo      # scripted walkthrough: two feeds, eight failure scenarios, self-checking
make replay    # replay the committed capture and verify its output digest
```

Run the whole pipeline in one process, on an in-memory log and store:

```sh
go run ./src/cmd/mdp standalone            # gateway on http://127.0.0.1:8080
curl -s 'http://127.0.0.1:8080/v1/instruments?search=xyz'
curl -s 'http://127.0.0.1:8080/v1/trades?instrument_id=venueA:XYZ&limit=5'
```

Run it as separate services on Redpanda and ClickHouse (Docker):

```sh
make stack-up          # builds the image; starts broker, store, simulator, 4 services, Prometheus
make failure-demo      # kills and stops things one at a time; audits ClickHouse after each
make integration-test  # adapters and pipeline against the real broker and store
make clean             # remove this project's containers and volumes
```

`make help` lists every target.

## What the demo shows

`make demo` runs the real services in one process and checks each claim as it
goes; it exits non-zero if any check fails. Recorded output:
[docs/evidence/demo.txt](docs/evidence/demo.txt).

1. Two feeds with different wire formats, units and sequence schemes become one schema.
2. A dropped connection resumes with no gap and no duplicate.
3. Events lost for good are declared as a gap: in the log, to WebSocket subscribers, and in `GET /v1/gaps`.
4. Duplicates are dropped; garbage and off-tick prices are dead-lettered with a reason.
5. Clock skew and stale events are flagged, never rewritten.
6. A silent feed turns `stale` on the gateway's own clock.
7. A store outage becomes consumer lag and `503`s, then drains with nothing lost or doubled.
8. A normalizer crash redelivers events; stored rows and WebSocket subscribers do not see repeats.

`make failure-demo` does the equivalent against real containers (SIGKILL,
stopped ClickHouse, restarted broker) and audits the store after each step.
Recorded output: [docs/evidence/failure-demo.txt](docs/evidence/failure-demo.txt).

## API

Served by the gateway. Prices and sizes are integers: multiply `price_ticks`
by the instrument's `tick_size` and `quantity` by its `lot_size` (both are
returned with every query). Timestamps are RFC 3339 UTC, and every response
that has them includes a `clocks` object naming the clock of each.

| Endpoint | Purpose |
| --- | --- |
| `GET /v1/instruments?search=` | reference data; case-insensitive substring of the instrument id |
| `GET /v1/trades?instrument_id=&from=&to=&limit=&cursor=` | trades with `event_time` in `[from, to)` |
| `GET /v1/quotes?…` | top-of-book quotes, same parameters |
| `GET /v1/depth?…` | depth snapshots, same parameters |
| `GET /v1/gaps?instrument_id=&from=&to=` | declared sequence gaps affecting the instrument |
| `GET /v1/health` | store reachability, ingest health per provider, freshness per instrument |
| `WS /v1/stream?instrument_id=a,b&channels=trade,quote,depth` | live events, gap notices, periodic status |
| `GET /metrics`, `GET /healthz` | Prometheus metrics, liveness |

`from` defaults to one hour before `to`, `to` to now, `limit` to 1,000 (max
10,000). When a page is full the response has a `next_cursor`; pass it back as
`cursor` to continue without skipping or repeating rows. Responses state the
table's retention and the oldest queryable day.

A trade as returned by the API and sent over the WebSocket:

```json
{"type":"trade","instrument_id":"venueA:XYZ","trade_id":"A-6","price_ticks":15005,"quantity":90,
 "aggressor":"buy","event_time":"2026-11-24T06:50:45.960903Z","receive_time":"2026-11-24T06:50:45.961035Z",
 "provider":"alpha","session":"A-S1","seq_domain":"XYZ","seq":5,"source":"live","normalizer_version":1}
```

WebSocket messages are JSON text frames with a `type`: `subscribed` (once),
`trade` / `quote` / `depth`, `gap`, and `status` every second.

```json
{"type":"status","server_time":"…","stale_after_ms":3000,"instruments":[
  {"instrument_id":"venueA:XYZ","last_event_time":"…","last_receive_time":"…","age_ms":127,"stale":false,"feed_gaps":0,"dropped":0}]}
{"type":"gap","reason":"feed_gap","provider":"beta","venue":"venueB","seq_domain":"main","from_seq":21,"to_seq":23,"missing":3,"receive_time":"…"}
{"type":"gap","reason":"slow_consumer","instrument_id":"venueA:XYZ","dropped":17}
```

- `age_ms` / `stale`: **freshness**, on the gateway's clock.
- `gap` with `feed_gap`: the **upstream feed** lost those sequence numbers.
- `gap` with `slow_consumer`, and `dropped` in `status`: **this connection**
  missed that many events because it read too slowly.

Errors are `{"error":{"code":"…","message":"…"}}` with `400 BAD_REQUEST`,
`401 UNAUTHENTICATED`, `403 NOT_ENTITLED`, `404 UNKNOWN_INSTRUMENT` or
`503 STORE_UNAVAILABLE`.

The gateway is open unless an entitlements file is configured; see
[examples/README.md](examples/README.md#entitlements).

## Delivery guarantees

Details and reasoning: [docs/architecture.md](docs/architecture.md).

- **At least once on every hop.** No hop claims exactly-once delivery. Each
  service does its work, then records progress, so a crash repeats work and
  never skips it.
- **Deduplication keys** make the repeats harmless. They are the ClickHouse
  sorting keys, and every query uses `FINAL`:

  | Fact | Key |
  | --- | --- |
  | trade | `instrument_id, event_time, trade_id` |
  | quote, depth | `instrument_id, event_time, seq` |
  | gap | `provider, session, seq_domain, from_seq` |

- **Loss is recovered or declared.** A skipped provider sequence number
  triggers one resend request; what the provider cannot resend is published as
  a gap record. It is not skipped silently.
- **Backpressure is lag, not drops.** A slow store or broker makes consumer
  lag grow (`mdp_consumer_lag_records`) while data waits in the log. The one
  place events are dropped is a WebSocket client that reads too slowly, and
  that client is told how many it missed.
- **Ordering** is per provider sequence domain and per instrument. There is no
  total order across venues.
- **Timestamps** name their clock: `event_time` is the venue's, `receive_time`
  the ingestor's, `persist_ts` the store's. They are never mixed.
- **Retention**: trades 90 days, quotes 30, depth 7, gaps 90; log topics 24 h
  (raw) and 7 days (canonical). Accepted timestamps: after 1970-01-01 and
  before 2100-01-01 UTC.
- **Units**: integer ticks and lots. No floating point between the wire and
  the store.

## Metrics

| Brief asks for | Metric |
| --- | --- |
| events per second | `rate(mdp_ingest_events_total[1m])` by `provider`, `kind` |
| bytes per second | `rate(mdp_ingest_bytes_total[1m])` |
| end-to-end ingestion latency p50/p99 | `mdp_pipeline_latency_seconds` histogram, stages `normalized`, `stored`, `delivered`; measured from `receive_time` |
| consumer lag | `mdp_consumer_lag_records` by `consumer`, `topic`, `partition` |
| replay catch-up time | `mdp_consumer_catchup_seconds` by `consumer` |
| gaps | `mdp_gaps_total`, `mdp_gap_missing_events_total` |
| duplicates | `mdp_ingest_duplicates_total`, `mdp_duplicates_total`, `mdp_ws_duplicates_suppressed_total` |
| reconnect frequency | `mdp_ingest_reconnects_total`, `mdp_ingest_resyncs_total` |

Also: `mdp_feed_lag_seconds` (venue-to-ingestor, spans two clocks),
`mdp_normalizer_rejects_total{reason}`, `mdp_sink_retrying`,
`mdp_sink_rejected_rows_total`, `mdp_ws_dropped_total`,
`mdp_flagged_events_total{flag}`.

Measured figures from this machine are in
[docs/benchmarks.md](docs/benchmarks.md). They describe one run on one laptop
with simulated load and are not a capacity claim.

## Implemented vs. planned

### Implemented

| Brief | Status |
| --- | --- |
| At least two schema-compatible feed adapters | `alpha` (JSON over WebSocket, per-symbol sequences, decimal strings) and `beta` (pipe-delimited TCP, one feed-wide sequence, scaled integers), plus a `replay` adapter for capture files |
| Canonical trade, quote, optional depth | all three; depth is snapshots of the best levels, from `beta` |
| Durable topic partitioning by instrument, with sequence metadata | canonical topics keyed by `instrument_id`; every event carries provider, session, sequence domain and number |
| ClickHouse ingest and a historical query endpoint | sink service; `GET /v1/trades`, `/quotes`, `/depth`, `/gaps` with keyset pagination |
| WebSocket subscriptions with freshness, dropped-event and gap indicators | `WS /v1/stream` |
| Services `ingestors`, `normalizer`, `stream`, `storage`, `gateway` | `src/` packages of those names, one `mdp` binary with a subcommand per service |
| Domain model `Instrument`, `MarketTrade`, `MarketQuote`, `FeedOffset`, `IngestHealth` | `src/domain` |
| Scenario tests: missing data, clock skew, stale events, connection loss, duplicate delivery, rejected writes | `tests/scenarios`, and against real services in `tests/integration` |
| Replay tests with reproducible results | committed capture and digest; `make replay` |
| Stretch: entitlement-aware gateway | per-venue bearer tokens; the file holds digests only |

### Partly implemented

| Item | What exists | What does not |
| --- | --- | --- |
| Stretch: tiered storage | `mdp archive` exports one day of trades and quotes to Parquet | no automatic tiering, no object storage, depth is not exported, and queries do not read the archive |
| Horizontal scale | partitioned topics, stateless gateway | one instance per service; no consumer-group rebalancing |
| Operations | Prometheus scrape config, runbook with alert conditions | no alert rules or dashboards |
| CI | `.github/workflows/ci.yml`, checked with `actionlint` | **never executed**: this directory is not a git repository and has no remote. Every command it runs was run locally |

### Planned, not implemented

- Schema registry (stretch goal). Messages are JSON with a
  `normalizer_version`; nothing enforces compatibility between versions.
- Real provider adapters. Out of scope by the brief's non-goals; the adapter
  interface (`src/ingestors/event.go`) is where one would go.
- Order-book reconstruction from incremental updates.
- TLS and rate limiting on the gateway.
- Versioned reference data (a tick-size change re-interprets stored integers).
- Detecting loss at the very end of a session (the feed heartbeat that would
  bound it is parsed and ignored).

Known limitations are listed in full in
[docs/architecture.md](docs/architecture.md#known-limitations).

## Definition of done

| Requirement | Where it is met | Evidence |
| --- | --- | --- |
| Restart or replay does not silently duplicate persisted facts | deduplication keys as store sorting keys; `FINAL` on every read | [integration tests](docs/evidence/integration-tests.txt), [failure demo](docs/evidence/failure-demo.txt) |
| At-least-once behaviour and dedup keys documented | this file, [architecture](docs/architecture.md#delivery-semantics-and-deduplication-keys), [ADR 0001](docs/decisions/0001-at-least-once-with-idempotent-keys.md) | |
| Backpressure produces observable lag, not silent drops | sink and ingestor block and retry; WebSocket drops are counted and reported in-band | scenario and integration outage tests, failure demo step 3 |
| All timestamps identify clock/source; query retention is defined | `clocks` and `retention` in every response; column comments in ClickHouse | `TestHistoricalQueryPaginatesWithoutLossOrRepeat`, `TestClickHouseSchemaAndDeduplication` |

The recorded test runs and coverage report are in [docs/evidence/](docs/evidence/).

## Layout

```
src/
  cmd/mdp/      the binary: services, standalone, demo, bench, fixture, replay, archive
  domain/       canonical schema, deduplication keys, accepted timestamp range
  fixed/        exact decimal-to-integer conversion
  refdata/      instruments and providers
  ingestors/    alpha, beta and replay adapters; reconnect, resume, resync
  seqtrack/     in-order / duplicate / gap / reset classification
  normalizer/   validation, unit conversion, gap declaration, checkpointing
  stream/       log interface; Kafka and in-memory implementations; partitioning; lag
  storage/      store interface; ClickHouse and in-memory implementations; sink; Parquet export
  gateway/      HTTP API, WebSocket hub, entitlements
  simulator/    the simulated venues and fault injection
  metrics/      Prometheus metrics
  app/          wiring shared by the commands and the tests
tests/
  unit/         packages in isolation, fuzz targets
  scenarios/    whole pipeline in process, with injected failures
  contract/     one suite per interface, run against both implementations
  integration/  real Redpanda and ClickHouse
examples/       reference data, capture, entitlements template
scripts/        failure demo, evidence collection, Prometheus config
docs/           architecture, decisions, runbook, test report, benchmarks, evidence
```

## Safety

- Simulated data only by default. No real-money connectivity exists in this
  codebase.
- The only secret is the ClickHouse password, read from the environment.
  `make setup` generates a random local one into `.env`, which git ignores.
  It is sent in an HTTP header and never logged. No credentials are committed.
- Compose publishes every port on `127.0.0.1` only.
- Performance figures are measurements from one machine, recorded with the
  command that produced them. Treat them as illustrative.
- Do not redistribute proprietary market data through this project; the
  entitlement check is per venue and is not a licensing control.
