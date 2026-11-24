# Rust Matching Engine

> A deterministic, price-time-priority exchange core with reproducible latency benchmarks.

**Difficulty:** Expert  
**Estimated build window:** 6–10 weeks (illustrative, solo development)  
**Stack:** Rust 1.99 (pinned in `rust-toolchain.toml`), Tokio only in the gateway, serde/serde_json, proptest, criterion  
**Status:** MVP implemented and tested — see [Implemented vs. Planned](#implemented-vs-planned). Simulated / paper only.

## Quick start

```bash
make setup             # pinned toolchain + dependencies
make lint              # rustfmt --check, clippy -D warnings
make test              # matching-core unit + property tests
make integration-test  # journal/recovery, TCP gateway, failure scenarios, replay fixtures
make demo              # simulated walkthrough + byte-identical replay proof
make failure-demo      # kill -9 the gateway, tear the log, recover
make bench             # writes docs/benchmarks/latest.md
```

```bash
# the example from this README, through the CLI
cargo run -- run examples/session.jsonl
# the gateway (loopback only; no auth)
cargo run --release -- serve --addr 127.0.0.1:9001 --data-dir data
```

Docs: [architecture](docs/architecture.md) · [ADR 0001](docs/decisions/0001-single-writer-core-with-command-sourcing.md) · [runbook](docs/runbook.md) · [benchmark report](docs/benchmarks/latest.md)

## Implemented vs. Planned

| Area | Status | Code | Evidence |
|---|---|---|---|
| Limit + market orders, partial/full fills, price-time priority | Implemented | [`src/matching_core/`](src/matching_core) | [`tests/unit/semantics.rs`](tests/unit/semantics.rs) |
| Cancel, reject, cancel/replace (priority kept on size-down, lost otherwise) | Implemented | [`engine.rs`](src/matching_core/engine.rs) | same |
| Trade, execution, top-of-book events with gap-free sequence ids | Implemented | [`types.rs`](src/matching_core/types.rs) | same |
| Invariants: never crossed, no zero/negative qty, FIFO per level | Implemented, checked after every command in tests | [`book.rs`](src/matching_core/book.rs) | [`tests/unit/properties.rs`](tests/unit/properties.rs) (engine vs. naive reference model) |
| Sequencer with monotonic venue time | Implemented | [`src/sequencer.rs`](src/sequencer.rs) | [`tests/scenarios/failures.rs`](tests/scenarios/failures.rs) |
| Write-ahead command journal, event journal, snapshots, recovery | Implemented | [`src/journal.rs`](src/journal.rs), [`src/pipeline.rs`](src/pipeline.rs) | [`tests/integration/recovery.rs`](tests/integration/recovery.rs) |
| Deterministic, byte-identical replay | Implemented | `me replay` | [`tests/scenarios/replay_fixtures.rs`](tests/scenarios/replay_fixtures.rs), `make replay-check` |
| TCP gateway (JSON lines), single-writer matching thread | Implemented | [`src/gateway.rs`](src/gateway.rs) | [`tests/integration/gateway.rs`](tests/integration/gateway.rs) |
| Seeded load generator, latency/throughput/allocation report | Implemented | [`src/loadgen.rs`](src/loadgen.rs), [`src/bin/me-bench.rs`](src/bin/me-bench.rs) | [`docs/benchmarks/latest.md`](docs/benchmarks/latest.md) |
| Multiple symbols | Implemented in one engine/one sequencer | | `symbols_are_isolated` |
| Dockerfile / docker-compose / GitHub Actions CI | Written, **not yet run** (no Docker daemon or CI runner was available during development) | [`Dockerfile`](Dockerfile), [`.github/workflows/ci.yml`](.github/workflows/ci.yml) | — |
| Sharding symbols across sequencers | Planned | | |
| Incremental market-data feed with snapshot recovery for subscribers | Planned (events go only to the submitting client and `events.jsonl`) | | |
| Alternative book structures with comparative flamegraphs | Planned | | |
| Group commit / benchmarked `--fsync` mode, log compaction, per-record checksums | Planned | | |
| Authn/authz, pre-trade risk, self-trade prevention, TIF flags, cancel-on-disconnect | Planned (out of MVP scope) | | |
| Gateway receive timestamps alongside venue time; end-to-end (TCP) latency benchmark | Planned | | |
| Binary protocols (UDP/Aeron/gRPC) | Planned | | |

### Test report

Run on 2026-11-23, macOS arm64, rustc 1.99.0: `cargo test` → **54 passed, 0 failed**
(31 unit/property incl. 4 proptest properties × 400 cases, 10 integration, 11 scenario, 2 lib);
`make lint` clean; `make replay-check` byte-identical for both fixtures.
As a sanity check on the tests themselves, deliberately breaking replace priority
made 3 tests fail (reverted).

### Benchmark summary

From [`docs/benchmarks/latest.md`](docs/benchmarks/latest.md) — Apple M5, macOS 26.5.1, rustc 1.99.0,
`opt-level=3`, fat LTO, 1 codegen unit; 2,000,000 measured commands after 200,000 warm-up,
seed 42, 4 symbols, 60% limit / 5% market / 25% cancel / 10% replace; matching core only
(no network, no journal):

| Throughput | p50 | p99 | p99.9 | Allocated bytes/command | Replay |
|---|---|---|---|---|---|
| 17.3 M commands/s | 42 ns | 250 ns | 1,083 ns | 0.90 | PASS |

Read these with the report's caveats: the OS timer step on this machine is 41 ns, so p50 is
about one timer tick; it is a single run on a non-isolated laptop, and an earlier run of the
same code measured 20.4 M/s, p99 125 ns, p99.9 875 ns. About a quarter of the generated
commands are cancels/replaces of already-filled orders and are rejected, which is cheap.
Journaled and over-TCP latency are not measured.

## Problem and outcome

Implement a miniature exchange matching engine that matches limit and market orders by **price-time priority**. Make the matching core deterministic and single-writer; measure latency at the engine boundary before adding networking.

## System architecture

```text
TCP/CLI load generator
         |
Command decoder + validation
         v
Sequenced input log
         |
Single-writer matching loop
         |
Per-symbol bid / ask books
         |
Execution + book events
         v
Journal / snapshots / downstream feed
```

## Minimum viable product (MVP)

- Add limit and market orders with unique order IDs.
- Partial/full fills, cancel, reject and cancel/replace semantics.
- Bid book descending, ask book ascending; FIFO within each price level.
- Trade, execution and top-of-book events with monotonic sequence IDs.
- Deterministic replay of an input log into the same event stream.

### Explicit non-goals

- Connecting to a real exchange.
- Production-grade exchange regulation or clearing/settlement.
- Claiming benchmark results without measurement and hardware details.

## Services and responsibilities

- `matching-core`: pure deterministic book state and command processing.
- `sequencer`: assigns total order and timestamps to commands.
- `journal`: append-only command/event log and snapshots.
- `gateway`: protocol adapter for clients and load tests.
- `bench`: traffic generator, histograms and reproducible reports.

## Domain model (starting point)

```text
Order(order_id, account_id, symbol, side, price_ticks, remaining_qty, seq)
PriceLevel(price_ticks, queue_of_order_ids, aggregate_qty)
Command(seq, kind, payload)
Execution(trade_id, maker_order_id, taker_order_id, qty, price_ticks)
BookEvent(seq, symbol, bid_px, bid_qty, ask_px, ask_qty)
```

Adjust schemas as requirements sharpen. Record timestamps in UTC with venue/event time versus local receive time clearly distinguished, and version decisions/rules where possible.

## API or protocol sketch

One JSON object per line. Prices are integer ticks, quantities integer lots.
`order_id` is a client-chosen unsigned integer (the original sketch used strings
such as `"o-1"`; integers were chosen so the core never allocates per order).

```text
{"type":"NEW","order_id":1,"symbol":"XYZ","side":"BUY","qty":100,"price_ticks":10025}
{"type":"NEW","order_id":2,"symbol":"XYZ","side":"SELL","qty":40,"price_ticks":10025}
{"type":"CANCEL","order_id":1}
```

Actual output of `cargo run -- run` for those three lines (also pinned in
`tests/scenarios/fixtures/`): an execution for 40 at 10025, 60 remaining, then the cancel.

```text
{"seq":1,"cmd_seq":1,"ts_ns":1,"type":"ACCEPTED","order_id":1,"account_id":0,"symbol":"XYZ","side":"BUY","ord_type":"LIMIT","qty":100,"price_ticks":10025}
{"seq":2,"cmd_seq":1,"ts_ns":1,"type":"BOOK_TOP","symbol":"XYZ","bid_px":10025,"bid_qty":100,"ask_px":null,"ask_qty":0}
{"seq":3,"cmd_seq":2,"ts_ns":2,"type":"ACCEPTED","order_id":2,"account_id":0,"symbol":"XYZ","side":"SELL","ord_type":"LIMIT","qty":40,"price_ticks":10025}
{"seq":4,"cmd_seq":2,"ts_ns":2,"type":"TRADE","trade_id":1,"symbol":"XYZ","price_ticks":10025,"qty":40,"maker_order_id":1,"taker_order_id":2,"taker_side":"SELL"}
{"seq":5,"cmd_seq":2,"ts_ns":2,"type":"EXECUTION","order_id":1,"trade_id":1,"role":"MAKER","side":"BUY","price_ticks":10025,"qty":40,"leaves_qty":60}
{"seq":6,"cmd_seq":2,"ts_ns":2,"type":"EXECUTION","order_id":2,"trade_id":1,"role":"TAKER","side":"SELL","price_ticks":10025,"qty":40,"leaves_qty":0}
{"seq":7,"cmd_seq":2,"ts_ns":2,"type":"BOOK_TOP","symbol":"XYZ","bid_px":10025,"bid_qty":60,"ask_px":null,"ask_qty":0}
{"seq":8,"cmd_seq":3,"ts_ns":3,"type":"CANCELED","order_id":1,"symbol":"XYZ","canceled_qty":60,"reason":"USER_REQUESTED"}
{"seq":9,"cmd_seq":3,"ts_ns":3,"type":"BOOK_TOP","symbol":"XYZ","bid_px":null,"bid_qty":0,"ask_px":null,"ask_qty":0}
```

Other commands:

```text
{"type":"NEW","order_id":3,"account_id":7,"symbol":"XYZ","side":"BUY","ord_type":"MARKET","qty":50}
{"type":"REPLACE","order_id":1,"qty":80,"price_ticks":10020}
```

`REPLACE.qty` is the new remaining quantity. Over TCP each command's events are followed by
`{"type":"DONE","cmd_seq":N,"events":K}`; undecodable lines get
`{"type":"ERROR","message":"..."}` and are never sequenced.
Full rules: [docs/architecture.md](docs/architecture.md).

## Suggested repository layout

```text
.
├── README.md
├── Cargo.toml / Cargo.lock / rust-toolchain.toml / build.rs
├── Makefile
├── src/
│   ├── matching_core/      # types, book, engine (pure, deterministic)
│   ├── sequencer.rs        # total order + venue time
│   ├── journal.rs          # JSONL logs, snapshots, replay, recovery
│   ├── pipeline.rs         # sequencer -> write-ahead log -> engine
│   ├── gateway.rs          # TCP line protocol (only async code)
│   ├── loadgen.rs, stats.rs
│   └── bin/me.rs, bin/me-bench.rs
├── tests/
│   ├── unit/               # semantics + property tests vs. reference model
│   ├── integration/        # recovery on disk, gateway over TCP
│   └── scenarios/          # failure scenarios + pinned replay fixtures
├── benches/matching.rs     # criterion
├── examples/session.jsonl
├── scripts/demo.sh, failure-demo.sh
├── docs/architecture.md, runbook.md, decisions/, benchmarks/
├── Dockerfile, docker-compose.yml
└── .github/workflows/ci.yml
```

## Getting started (implementation checklist)

1. ✅ Repository layout, crate `matching-engine`, toolchain pinned to Rust 1.99.0, `Cargo.lock` generated.
2. ✅ `Makefile` with `setup`, `lint`, `test`, `integration-test`, `demo` (plus `failure-demo`, `bench`, `replay-check`, `fixtures`).
3. ✅ Local simulator: seeded load generator (`me gen`) and fixtures; the project uses no credentials at all.
4. ✅ End-to-end vertical slice: TCP → sequencer → journal → matching → events → reply.
5. ✅ Deterministic replay fixtures, benchmark metrics, ADR. The CI workflow is written but has not run yet.

## Milestone roadmap

1. ✅ Price/quantity integer model, book structures and invariants.
2. ✅ Matching, market orders, cancels, replace and event schemas.
3. ✅ Journals, snapshots, deterministic replay and property tests.
4. ◐ Gateway and latency bench with performance report are done; memory profiling is limited to allocator counters (no heap profiler or flamegraphs yet).

## Testing strategy

- **Unit:** validation, state transitions, numeric boundaries and all documented invariants.
- **Integration:** API, state store, message transport and simulator under retries/restarts.
- **Scenario:** missing data, clock skew, stale events, connection loss, duplicate delivery and rejected writes.
- **Replay:** fixed fixtures and seeds produce reproducible outputs; pin versions of dependencies and datasets.
- **Performance:** report realistic load shape, machine details, p50/p99 where relevant, errors and memory usage (do not report fabricated metrics).

## Definition of done

- ✅ For every fill, the trade price and maker/taker IDs are deterministic — property test against a reference model; pinned fixtures.
- ✅ No negative remaining quantity, crossed resting book or lost price-level FIFO — `check_invariants` after every command in tests.
- ✅ Replaying the same sequenced commands yields byte-equivalent normalized events — `make replay-check`, `replay_is_byte_identical`.
- ✅ Benchmark reports include CPU, OS, build flags, load profile, throughput and p50/p99/p99.9 — `docs/benchmarks/latest.md`.

## Metrics to expose

All four are produced by `make bench` (an offline report; there is no live metrics endpoint yet):

- Commands processed per second
- Matching-core p50/p99/p99.9 latency
- Allocated bytes per command
- Replay consistency and memory footprint

## Stretch goals

- Multiple symbols sharded by deterministic sequencers.
- Market-data incremental feed with snapshot recovery.
- Alternative data structures with comparative flamegraphs.

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

No license has been chosen yet, so this code is not licensed for reuse and `publish = false`
is set in `Cargo.toml`. Choose one before publishing; MIT or Apache-2.0 are common for
open-source software, subject to dependency and data-license compatibility.
