# Architecture

All market data in this repository is **simulated**. Nothing connects to a venue or broker.

```text
              feed-adapter                         book-core            analytics
        ┌──────────────────────┐        ┌───────────────────────┐   ┌─────────────┐
        │ SimSession (seeded)  │        │ SequencedBook         │   │ BookMetric  │
        │  true book + faults  │  raw   │  seq check, reorder   │   │ BestQuote   │
        │ capture file replay  │ frames │  buffer, resync FSM   │   │ OFI tracker │
        │ Normalizer           │───────►│ OrderBook<Ladder>     │──►│             │
        │  decimal → ticks/lots│        │  integrity, checksum  │   └──────┬──────┘
        └──────────▲───────────┘        └───────────▲───────────┘          │
                   │ snapshot request               │                      │
                   │                     ┌──────────┴──────────────────────▼─────┐
                   └─────────────────────│ stream-api: Engine (single writer)    │
                                         │ pump · HTTP · WebSocket · /metrics    │
                                         └───────┬───────────────────┬───────────┘
                                       broadcast │                   │ bounded queue
                                                 ▼                   ▼
                                          ui (React depth       ClickHouse sink
                                          ladder, alerts)       (optional)
```

## Crates

| Path | Responsibility | I/O |
|---|---|---|
| `src/book-core` | Level-2 book, ladder implementations, sequence state machine, checksum | none |
| `src/analytics` | Spread, mid, microprice, imbalance, depth, order-flow imbalance | none |
| `src/feed-adapter` | Venue wire format, exact decimal parsing, simulator with fault injection, capture files | files |
| `src/stream-api` | `Engine`, HTTP/WebSocket server, Prometheus text, ClickHouse sink, `lob` CLI | network |
| `src/ui` | Depth ladder, top-of-book tiles, feed health, event timeline, fault controls | browser |
| `tests` | Unit, integration and scenario suites as one crate (`lob-tests`) | — |

`book-core`, `analytics` and `Engine` never read a clock or touch the network. Receive time is an
argument. That is what makes a capture replay to byte-identical output and lets staleness be tested
without sleeping.

## Data flow

1. A **source task** (live simulator or capture replay) hands raw frames to the pump over a bounded
   channel, each stamped with the local receive time and a monotonic `Instant`.
2. The **pump** drains up to 256 frames per lock acquisition into the `Engine`, which decodes,
   sequences and applies them, returning `Output`s: applied deltas, new base images, status events
   and resync requests.
3. Resync requests go back to the source. Everything else is broadcast to WebSocket clients and
   offered to the ClickHouse sink.
4. Every 100 ms the pump publishes a book image and metrics per symbol; every 500 ms, feed health.
   These keep flowing when the feed is silent, which is how staleness reaches the screen.

## Sequence invariants and recovery

`SequencedBook` (see `src/book-core/src/sequencer.rs`) enforces:

| Situation | Behaviour |
|---|---|
| `seq == last + 1` | apply |
| `seq <= last` | duplicate: ignore, count |
| `seq > last + 1` | **gap**: state → `resyncing`, buffer the delta, request a snapshot |
| delta that fails validation at `last + 1` | reject, state → `resyncing` (its sequence number was consumed) |
| snapshot with `seq >= last` | replace book, drop buffered deltas `<= seq`, apply the rest in order; `synced` only if they chain |
| snapshot with `seq < last` | stale: ignore, keep waiting |
| snapshot with `seq == last` while synced | compare with the book; a difference is **drift** (counted, snapshot wins) |
| missing delta arrives late while resyncing | apply it and the buffer; if everything chains, `synced` without a snapshot |
| source restart | `reset()`: back to `awaiting_snapshot` |

While a book is not `synced`: no deltas and no metrics are published for it, images carry
`state: "resyncing"`, and the snapshot request is repeated every `--resync-retry-ms`. The reorder
buffer is bounded (50 000 deltas); overflow drops the oldest and is counted.

Crossed or locked books are legal inputs (a venue can publish them) and are passed through, never
"repaired". `integrity` on every image and health report says `ok`, `locked` or `crossed`, and each
transition emits a status event.

## Time

Three clocks exist and are never mixed silently:

| Field | Clock | Used for |
|---|---|---|
| `event_ts`, `captured_at`, `ts` | venue | display, storage |
| `recv_ts`, `asof`, status `ts` | this service (UTC, microseconds) | staleness, recovery time |
| browser `performance.now()` | browser, monotonic | "have I heard from the server?" watchdog |

* **Staleness** is `now − last receive time` on the service clock. Venue timestamps cannot make
  stale data look fresh or the reverse (tested with skewed and hour-old venue timestamps).
* **`venue_lag_ms`** (`recv − event`) subtracts two unsynchronized clocks. It is exposed as an
  indication, labelled as such in the API docs and UI, and flagged as `clock_skew_suspected` when
  the venue is ahead of the local clock by more than the tolerance.
* **Latency metrics** (`lob_feed_to_book_seconds`, `lob_gap_recovery_seconds`) are measured inside
  one process on one clock. No end-to-end latency is claimed.

## Numbers

Prices are `i64` ticks and sizes `u64` lots. The venue sends decimal strings; `decimal.rs` converts
them exactly and rejects values that are not whole multiples of the tick or lot. JSON numbers for
price are refused outright. Floating point appears only in derived analytics (ratios, microprice).

## HTTP and WebSocket API

| Route | Purpose |
|---|---|
| `GET /v1/symbols` | Instruments with tick and lot size |
| `GET /v1/books/{symbol}/snapshot?depth=20` | Book image (`depth` 1…`--max-depth`) |
| `GET /v1/books/{symbol}/quote` | Best bid/ask plus state, integrity and staleness |
| `GET /v1/books/{symbol}/health` | `FeedHealth` |
| `WS  /v1/books/{symbol}/stream?depth=20&deltas=false` | Live stream (below) |
| `GET /v1/source` | Active source, whether data is simulated, replay speed |
| `GET /metrics` | Prometheus text format |
| `POST /v1/admin/faults` | Inject a simulator fault (needs `--admin`, sim source) |
| `POST /v1/admin/replay` | `{"speed": 5, "paused": false}` (needs `--admin`, replay source) |

Stream messages are JSON objects with a `type`:

| `type` | When | Notes |
|---|---|---|
| `hello` | once, first | instrument, staleness threshold, source info |
| `snapshot` | on connect, after every resync, after drift, after the client lagged | new base image |
| `book` | every 100 ms | image cut to the client's `depth` |
| `delta` | per applied delta, only with `deltas=true` | normalized, e.g. below |
| `metric` | every 100 ms while synced | `BookMetric` |
| `health` | every 500 ms | `FeedHealth` |
| `status` | on events | `gap_detected`, `resynced`, `stale`, `fresh`, `crossed`, … |

```json
{"type":"delta","symbol":"XYZ","seq":10204,"side":"BID","price_ticks":10025,"quantity":350,"action":"SET","event_ts":1767225600000000}
```

Rules for a client that maintains its own book from deltas (`tests/integration/api.rs` does this and
checks its checksum against the server's):

* With `deltas=true`, `snapshot` messages carry the **full** book regardless of `depth`; a
  depth-limited base cannot be kept correct by deltas.
* Ignore any delta whose `seq` is not greater than the current base `seq`.
* A `snapshot` always replaces the local book. Between snapshots, deltas are gap-free.

A subscriber that falls behind the broadcast buffer is told (`status: client_lagged`) and re-based
with a fresh snapshot — the same recovery shape as the feed side.

## Storage

The ClickHouse sink is optional and strictly downstream: rows go through a bounded queue
(`try_send`), batches are retried with backoff and then dropped, and drops are counted. The book
never waits for storage. Schema: `scripts/clickhouse-schema.sql`.

## Not built (see README, Implemented vs. Planned)

Kafka/Redpanda transport, a real venue adapter, level-3 simulation and authentication. The pump →
broadcast boundary is where a message bus would sit; see ADR 0002.
