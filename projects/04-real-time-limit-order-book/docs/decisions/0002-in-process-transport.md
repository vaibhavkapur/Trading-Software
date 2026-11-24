# ADR 0002 — In-process channels instead of Kafka/Redpanda (for now)

Status: accepted · Date: 2026-11-23

The blueprint lists Kafka/Redpanda. This version uses a bounded `mpsc` channel from source to
engine and a `broadcast` channel from engine to subscribers.

**Why:** there is one producer and one consumer of raw frames, in one process. A broker would add an
operational dependency without changing any correctness property the project is about (sequencing,
recovery, staleness), and replay — the thing a log is usually wanted for here — is already covered
by capture files with pinned digests.

**What it costs:** no durable fan-out, no independent consumers, no replay from an offset for late
subscribers (they get a snapshot instead).

**When to revisit:** a second independent consumer of normalized events, or more than one process.
The seam is `server::dispatch`: publish `Output`s to a topic keyed by symbol there, and move the
WebSocket layer to a consumer that rebuilds from snapshot + deltas using the documented client rules.
