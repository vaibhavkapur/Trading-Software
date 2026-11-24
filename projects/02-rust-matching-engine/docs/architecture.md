# Architecture

Simulated / paper only. Nothing here connects to a real venue.

## Data flow

```text
 TCP clients (JSON lines)            CLI: me run / me replay / me-bench
          |                                        |
   gateway (tokio)                                 |
   decode + size limit                             |
          |  bounded mpsc (arrival order)          |
          v                                        v
 +-------------------- single OS thread: "matching-loop" --------------------+
 |  Sequencer        assigns seq (gap-free) + venue ts_ns (non-decreasing)   |
 |      |                                                                    |
 |  commands.jsonl   write-ahead, one write() per record  [--fsync optional] |
 |      |                                                                    |
 |  Engine::process  pure: (state, SequencedCommand) -> events               |
 |      |              per-symbol Book: BTreeMap<price, Level> x2 + slab     |
 |  events.jsonl     derived feed, rebuilt from the command log if behind    |
 |      |                                                                    |
 |  snapshots/       periodic + on clean shutdown (atomic rename)            |
 +---------------------------------------------------------------------------+
          |
   reply to the submitting client: its events + {"type":"DONE",...}
```

## Modules

| README service | Code | Responsibility |
|---|---|---|
| `matching-core` | `src/matching_core/` (`types.rs`, `book.rs`, `engine.rs`) | Order book, matching, validation, events, snapshots, invariant checker. No I/O, no clock, no async. |
| `sequencer` | `src/sequencer.rs` | Total order and venue timestamps; the only place a clock is read. |
| `journal` | `src/journal.rs` | JSONL writer/reader, torn-tail repair, snapshots, `replay`, `recover`. |
| (glue) | `src/pipeline.rs` | Sequencer → write-ahead log → engine → event log; fail-stop on write errors. |
| `gateway` | `src/gateway.rs`, `src/bin/me.rs` | TCP line protocol; CLI. |
| `bench` | `src/loadgen.rs`, `src/stats.rs`, `src/bin/me-bench.rs`, `benches/matching.rs` | Seeded load, percentiles, report. |

## Matching rules

- **Price-time priority.** Bids are walked highest price first, asks lowest
  first; within a price level orders fill strictly in arrival order.
- **Trade price** is always the resting (maker) order's price.
- **Limit orders** match what crosses and rest the remainder.
- **Market orders** match until filled or the opposite side is empty; any
  remainder is canceled with reason `NO_LIQUIDITY`. They never rest.
- **Cancel** removes a resting order; unknown/filled ids are rejected.
- **Replace** (`qty` = new remaining quantity):
  - same price and `qty` ≤ remaining → amended in place, **priority kept**;
  - price change or size increase → removed and re-entered at the back of the
    queue with the same id, and **may trade immediately** if the new price
    crosses. `REPLACED.priority_retained` says which happened.
- **Order ids** are client-chosen `u64`, unique among *live* orders across all
  symbols. An id may be reused after its order is fully filled or canceled.
- **Validation** (rejects consume a sequence number and emit `REJECTED`):
  `qty` in `1..=1e9`, limit `price_ticks` in `1..=1e12`, limit needs a price,
  market must not have one, duplicate live id, unknown order.
- No self-trade prevention, no time-in-force other than the above, no
  auctions, no tick-size/lot-size tables. See "Implemented vs. Planned" in the
  README.

## Events

Every event carries `seq` (gap-free event sequence), `cmd_seq` and `ts_ns`
(copied from the causing command). Per command, in order:

1. `ACCEPTED` or `REJECTED` (new orders); `CANCELED`; `REPLACED`.
2. For each fill, in match order: `TRADE`, `EXECUTION` (maker), `EXECUTION`
   (taker). `trade_id` is gap-free.
3. `CANCELED` (`NO_LIQUIDITY`) for an unfilled market remainder.
4. `BOOK_TOP` iff the best bid/ask price or aggregate size changed.

## Invariants

Checked by `Engine::check_invariants` after every command in the unit and
property tests, and after every recovery:

- best bid < best ask (book never rests crossed);
- no resting order with zero quantity; quantities are unsigned and all
  subtraction is of a value ≤ the minuend (overflow checks are on in tests);
- each level's aggregate quantity and count equal the sum over its queue;
- within a level, priority stamps (`seq`) strictly increase head → tail;
- intrusive list links are consistent; no empty level remains;
- the id index and the books contain exactly the same orders.

## Determinism

- `Engine::process` reads no clock, does no I/O, uses no randomness.
- Hash maps are used for point lookups only and are never iterated; ordered
  traversal uses `BTreeMap` or explicit sorting (snapshots sort by symbol).
- Integers only; no floating point in the execution path.
- The engine refuses any command whose `seq` is not exactly `last_seq + 1`.
- Events serialise with a fixed field order, so "same log → same bytes" is
  testable with `cmp` (`make replay-check`, `tests/scenarios`).

## Durability and recovery

- A command is appended to `commands.jsonl` before it is applied. Default:
  the bytes are handed to the OS (survives a process crash). With `--fsync`
  each command is also `fsync`ed (survives power loss; far slower — not
  benchmarked here).
- A record is one `write` including its newline, so a crash can only leave a
  torn *final* line. Readers drop it and truncate the file; a bad line
  anywhere else is reported as corruption and recovery stops.
- Journal write failure ⇒ the command is not applied, the client gets an
  `ERROR`, and the pipeline halts until restarted.
- Recovery = newest snapshot that parses and validates + replay of log
  records with `seq` greater than the snapshot's. Records at or below the
  engine's sequence are skipped, so redelivery is idempotent; a gap is fatal.
- `events.jsonl` is derived data. On start, events missing from its tail are
  regenerated from the command log (events older than the snapshot used for
  recovery cannot be regenerated without a full replay).

## Time

`ts_ns` is **venue time**: assigned by the sequencer, UTC nanoseconds since
the Unix epoch, clamped to be non-decreasing even if the wall clock steps
back or the process restarts with a slow clock. The CLI and tests use a
logical clock so outputs are reproducible. Client send time and gateway
receive time are *not* recorded (planned).

## Security boundaries

The gateway has no authentication, authorisation, TLS, rate limiting or
pre-trade risk checks. It binds to loopback by default and should stay
there. Input lines are capped at 4096 bytes. Order-id hashing is fast, not
collision-resistant: do not expose it to untrusted clients.
