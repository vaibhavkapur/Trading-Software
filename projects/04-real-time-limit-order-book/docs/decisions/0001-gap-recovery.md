# ADR 0001 — Gap recovery: freeze, buffer, and rebase on a snapshot

Status: accepted · Date: 2026-11-23

## Context

A level-2 book built from incremental updates is only right if every update is applied exactly once
and in order. Feeds lose, duplicate and reorder messages. When a sequence gap appears there are
three plausible responses:

1. **Keep applying** later deltas and hope the missing ones did not matter.
2. **Clear the book** and show nothing until a snapshot arrives.
3. **Freeze** the last consistent book, mark it untrusted, buffer what arrives, and rebase on a
   snapshot.

## Decision

Option 3, with one refinement: if the missing delta shows up late (plain reordering), apply it and
the buffered run and resume without a snapshot.

* The frozen book stays visible but every image carries `state: "resyncing"`; the UI dims it and
  says why. Deltas and metrics are not published while unsynced.
* Deltas received during the gap are kept in a bounded, sequence-ordered buffer. After a snapshot at
  sequence `S`, buffered deltas `<= S` are discarded and the rest must chain from `S + 1`. If they do
  not, the book stays `resyncing` and the request is retried.
* A snapshot older than the book's own sequence is ignored rather than applied.
* A snapshot at exactly the book's sequence while synced is used as a free consistency check
  ("drift"): sequence numbers prove continuity, not correctness.

## Why not the alternatives

* **Keep applying** is the failure mode this project exists to avoid: the book would look healthy
  while being wrong, and nothing downstream could tell. Level-2 updates are absolute per level, so a
  lost update for a level leaves it wrong until that level happens to change again, which for deep
  levels may be never.
* **Clear the book** is correct but throws away information an operator wants during an incident
  ("what did we last know?") and makes a 60 ms recovery look like an outage. Marking beats hiding,
  provided the mark cannot be missed.

## Consequences

* Recovery time is bounded by the snapshot round trip, not by message volume, and is measured
  (`lob_gap_recovery_seconds`).
* Memory during a long gap is bounded by the buffer cap. On overflow the oldest deltas go first; if
  the eventual snapshot predates what survived, the hole is detected again and another snapshot is
  requested. Correctness never depends on the buffer being large enough.
* Subscribers must treat `snapshot` as a rebase. The same rule covers their own lag, so there is one
  recovery path to implement, not two.
* A capture replay cannot ask for snapshots. It recovers at the same point the recorded session did,
  because the answering snapshot is in the capture. Removing a frame from a capture therefore leaves
  that book `resyncing` until the next recorded snapshot — reported, not papered over.
* Costs accepted: a stale-but-marked book is on screen during recovery, and every consumer has to
  honour `state`. The integration suite asserts that nothing published between `gap_detected` and
  `resynced` claims to be synced.
