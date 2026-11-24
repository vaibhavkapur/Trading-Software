# ADR 0001: Single-writer core, command log as the source of truth

- Status: accepted
- Date: 2026-11-23

## Context

The definition of done requires that replaying the same sequenced commands
yields byte-equivalent events, and that maker/taker ids and prices are
deterministic for every fill. We also want crash recovery and a latency
figure for the matching core that is not polluted by I/O.

The hard trade-off is **what to persist and when**, relative to matching:

1. *State sourcing* – persist book state (or events) after matching.
2. *Command sourcing* – persist the sequenced input before matching and
   treat everything else as derived.

and **how many writers** touch a book.

## Decision

- One thread owns the sequencer, the journal writers and every book. Network
  tasks only decode and enqueue; the queue order is the total order.
- The sequenced command log is the single source of truth and is written
  ahead of applying a command. Events and snapshots are derived, disposable
  accelerators.
- The matching core is a pure state machine: no clock, no I/O, no iteration
  over unordered containers. It rejects any `seq` other than `last_seq + 1`.
- If the command log cannot be written, the pipeline halts (fail-stop).

## Consequences

Good:

- Determinism is structural rather than tested-in: there is no lock ordering,
  thread interleaving or wall-clock read inside the core to get wrong.
- Recovery, audit and "what did the engine do at seq N" are the same
  mechanism (replay). Snapshots can be deleted at any time.
- The core can be benchmarked and property-tested in isolation.

Costs we accepted:

- **Throughput is bounded by one core**, and one slow command delays all
  symbols. Sharding by symbol behind per-shard sequencers is the planned
  escape hatch; it is not built.
- **Latency of an acknowledged command includes a log write.** Without
  `--fsync` an OS crash can lose acknowledged commands; with it, every
  command pays an fsync. Group commit would be the fix; not built.
- **Rejects are logged and consume sequence numbers**, because whether a
  command is rejected depends on book state. Only undecodable input is
  dropped before sequencing.
- **Changing matching rules changes history.** Replaying an old log with new
  rules gives different events, so rule changes need a version boundary
  (snapshot + new log). The snapshot carries a `version`; the log does not
  yet.
- JSON Lines was chosen over a binary log for inspectability. It costs CPU on
  the gateway path (not in the core benchmark) and has no per-record
  checksum, so only torn tails and unparseable lines are detected, not
  bit-flips that still parse.

## Alternatives considered

- *Multi-threaded book with locks*: higher theoretical throughput, but
  ordering between a cancel and a fill becomes scheduling-dependent, which
  breaks byte-identical replay.
- *Persist events only*: cheap to tail, but recovery needs a state
  reconstruction path that is separate from the matching code and can drift
  from it.
