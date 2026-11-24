# 0001. At-least-once delivery with idempotent keys

Status: accepted

## Context

A market event passes five hops: provider → ingestor → raw topic → normalizer
→ canonical topics → store and live subscribers. Any process can crash between
doing its work and recording that it did. The specification requires that a
restart or a replay "does not silently duplicate persisted facts" and that the
delivery semantics be documented.

## Options

1. **Exactly-once with Kafka transactions.** Covers topic-to-topic hops
   (normalizer) only. The provider socket and ClickHouse are outside any Kafka
   transaction, so the first and last hops would still need another answer.
   It also ties correctness to one broker feature and makes the in-memory log
   used for hermetic tests either a transaction emulator or a liar.
2. **At-most-once** (record progress first, then work). Simple, and loses data
   on every crash.
3. **At-least-once on every hop, with a key at each hop that makes a repeat
   harmless.**

## Decision

Option 3.

- Each service does its work first and records progress second, so a crash
  repeats work and never skips it.
- Each hop has a deduplication key derived from the venue's own identifiers,
  not from anything the platform generates:
  - trades: `instrument_id, event_time, trade_id`
  - quotes and depth: `instrument_id, event_time, seq`
  - gaps: `provider, session, seq_domain, from_seq`
- Those keys are the ClickHouse sorting keys of `ReplacingMergeTree` tables,
  and every read uses `FINAL`, so a repeat is invisible whether or not a merge
  has happened yet.
- Provider-level repeats are dropped earlier, by sequence number, at the
  ingestor and again at the normalizer.

## Consequences

- The canonical topics can contain repeats after a normalizer crash. Any
  consumer of those topics must be idempotent on the keys above. The two in
  this repository (sink, gateway) are.
- `FINAL` costs query time. Tables are partitioned by day on the same
  timestamp the key contains, which keeps the merge work per query local to
  the partitions the range touches.
- Physical row counts in ClickHouse can exceed logical ones until merges run.
  Monitoring and audits must count with `FINAL`.
- A venue that reuses a `trade_id` at the same timestamp for two different
  trades would have them collapsed. The key trusts the venue's identifiers.

## Evidence

`TestRedeliveryIntoClickHouseIsIdempotent` rewinds the sink's committed
offsets on real Redpanda and ClickHouse: 400 rows become 800 on disk, queries
return 400, and 400 remain after `OPTIMIZE`. The failure demo audits the store
after six injected failures. See [../test-report.md](../test-report.md).
