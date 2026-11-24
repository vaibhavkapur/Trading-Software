# Test report

Run on 2026-11-23. Every figure below was produced by the commands shown, on the machine described;
nothing is estimated. All data is simulated.

**Machine:** Apple M5 (10 cores), 16 GB RAM, macOS 26.5.1 · rustc 1.99.0 · Node 20.20.2

## Pass / fail

| Command | Result |
|---|---|
| `make lint` (rustfmt check, clippy `-D warnings`, `tsc --noEmit`) | pass |
| `make test` | pass — 66 Rust tests, 8 UI tests |
| `make integration-test` | pass — 12 tests |
| `make replay` | pass — digest reproduced, 39/39 checkpoints matched |

The full Rust suite was additionally run five times in a row to look for timing flakiness in the
socket tests: 5/5 green.

| Suite | File | Tests | What it covers |
|---|---|---:|---|
| unit | `tests/unit/book.rs` | 12 | apply/validate, integrity, checksum, numeric boundaries |
| unit | `tests/unit/sequencer.rs` | 15 | every state transition of the gap/recovery machine |
| unit | `tests/unit/ladders.rs` | 3 | both ladders, incl. 100 000 random ops compared step by step |
| unit | `tests/unit/analytics.rs` | 7 | spread, mid, microprice, imbalance, OFI |
| unit | `tests/unit/wire.rs` | 9 | exact decimals, normalization, malformed frames, fault plans |
| scenario | `tests/scenarios/faults.rs` | 15 | loss, duplicates, reordering, corruption, crossed book, stale feed, connection loss, clock skew, restart — scored against the simulator's true book |
| scenario | `tests/scenarios/replay.rs` | 5 | determinism, checkpoint accuracy, tamper detection, pinned fixture |
| integration | `tests/integration/api.rs` | 9 | REST, WebSocket, client-side reconstruction, fault injection, stale alert, served replay |
| integration | `tests/integration/clickhouse.rs` | 3 | batching, rejected writes with retry, back-pressure |
| UI | `src/ui/src/logic.test.ts` | 8 | freshness decision, exact price formatting |

### Definition of done

| Requirement | Evidence |
|---|---|
| Never publishes a crossed or invalid state without flagging it | `crossed_book_is_always_flagged` checks, after every frame, that the published `integrity` agrees with the published levels. Invalid events: `invalid_in_sequence_delta_forces_a_resync`, `invalid_snapshot_leaves_book_untouched`. |
| Dropped or out-of-order deltas trigger a documented recovery path | `dropped_deltas_trigger_resync_and_recover`, `out_of_order_delivery_heals_from_the_reorder_buffer`, `injected_gap_is_reported_and_recovered_over_the_stream`; documented in `docs/architecture.md` and ADR 0001. |
| Replay of known events reproduces reference depth at checkpoints | `replay_reproduces_recorded_output_and_reference_depth`, `committed_fixture_replays_to_its_pinned_digest`, `make replay`. Negative control: `tampered_capture_fails_verification`. |
| UI displays feed freshness and does not silently present stale prices | Server side: `stalled_feed_raises_stale_alert_and_clears_on_recovery`. UI decision logic: `logic.test.ts`. Rendered result: `docs/images/stale-feed.jpg`. |

`many_mixed_faults_never_leave_the_book_wrong` is the broadest check: 8 seeds × 30 000 frames with a
random fault every 400 frames; whenever the engine reports `synced` at the venue's sequence number
its full book must equal the simulator's (more than 20 000 comparisons per seed), and no drift may
be recorded.

## Not verified

* The ClickHouse sink has only been exercised against a stand-in HTTP server. The schema in
  `scripts/clickhouse-schema.sql` and `docker-compose.yml` have **not** been run against a real
  ClickHouse (no Docker daemon was available).
* The `Dockerfile` has not been built, and the GitHub Actions workflow has not run.
* The UI was checked by hand in Chrome (live, stale, gap, crossed, corrupt-frame paths) in dark
  mode only; there are no automated browser tests.
* No real venue feed has been connected.

## Measurements

These describe this code on this machine with synthetic load. They are not capacity claims.

### Offline replay (decode + sequence + book + status, single thread)

```bash
make replay
```

Input: `examples/fixtures/session-faults.ndjson` — 20 002 frames, 2 symbols, ~20 levels per side,
JSON decoding included. Three runs:

| Run | frames/s | per-frame p50 | p99 | max |
|---|---:|---:|---:|---:|
| 1 | 862 559 | 1 084 ns | 1 833 ns | 126 500 ns |
| 2 | 1 000 602 | 958 ns | 1 125 ns | 36 708 ns |
| 3 | 943 402 | 1 000 ns | 1 209 ns | 94 625 ns |

Errors: 0 checkpoint mismatches; 1 decode error (the scheduled corrupt frame). Peak RSS 6.9 MB
(`/usr/bin/time -l`). Each per-frame sample includes two `Instant::now()` calls. The run is 20 ms
long, so the max is dominated by warm-up and scheduling noise.

### Live service, feed hand-off → book updated

```bash
lob serve --rate 5000        # 2 symbols, 20 s, no subscribers
curl -s localhost:8080/metrics | grep lob_feed_to_book_seconds
```

99 900 frames. Cumulative share at or under each bucket bound:

| ≤ 25 µs | ≤ 50 µs | ≤ 100 µs | ≤ 250 µs | ≤ 1 ms | ≤ 2.5 ms |
|---:|---:|---:|---:|---:|---:|
| 4.5 % | 62.3 % | 95.7 % | 99.88 % | 99.98 % | 100 % |

So the median lies in the 25–50 µs bucket and p99 in the 100–250 µs bucket. This interval includes
channel queueing and task wake-up, and the simulator emits in 5 ms bursts, so it reflects
scheduling more than book cost. It is in-process time on one monotonic clock — **not**
venue-to-screen latency. Process RSS at the end: 5.1 MB.

### Gap recovery time

In `scripts/failure-demo.sh` runs, recovery took 65 ms. That number is a property of the simulator
(a requested snapshot is delivered 25 events later; at 400 events/s that is ~62 ms), not of the
engine. It shows the measurement works, nothing more.

### Ladder comparison (stretch goal)

```bash
make bench     # 1 000 001 events per case, best of 5 rounds, already-decoded events
```

Workload: apply each event through the sequencer, then read best bid and best ask. Updates
concentrate near the touch.

| Levels per side | `btree` ns/event | `sorted-vec` ns/event |
|---:|---:|---:|
| 20 | 40.8 | 28.5 |
| 200 | 52.2 | 39.5 |
| 2 000 | 64.1 | 117.6 |

Final checksums were identical for both ladders in every case. On this workload the sorted vector
is faster for shallow and medium books and loses at 2 000 levels, where insertions far from the
touch shift many elements. `BTreeLadder` stays the default because its cost does not depend on
where in the book the update lands. Single machine, single run of the benchmark, no variance
analysis: treat the ordering as indicative.
