# 0003. The normalizer checkpoints its offset and its sequence state together

Status: accepted

## Context

The normalizer declares a gap when a provider sequence number is skipped, and
drops an event whose number it has already seen. Both decisions depend on
"the last sequence number seen per domain", which is state. After a restart
that state has to match the position in the raw topic the normalizer resumes
from, exactly:

- state ahead of the offset: reprocessed events look like duplicates and are
  dropped, including ones whose output was never published. Silent loss.
- state behind the offset: the first event after the restart looks like it
  follows a hole. A false gap, or a real one missed.

Kafka consumer-group offsets and any separately stored tracker are two writes
and can be torn by a crash between them.

## Decision

Per raw partition, the normalizer writes one record to the compacted
`md.checkpoints` topic containing both the next offset to read and the
sequence tracker as of that offset. It resumes from that record and nothing
else. Group offsets are still committed, but only so standard lag tooling
(`rpk group describe`) shows the normalizer's position.

Order within a batch: publish every output, then checkpoint. The normalizer
also checkpoints when idle, so on a quiet feed its saved position reaches the
log end instead of trailing by the last partial batch.

## Consequences

- A crash between publishing and checkpointing reprocesses the batch from the
  previous checkpoint with the previous tracker, and so republishes the same
  outputs with the same keys (see ADR 0001). Gaps are re-declared with the
  same key too.
- If the checkpoint topic is lost, the normalizer reprocesses the raw topic
  from its earliest retained offset with an empty tracker. Output is
  re-derived and deduplicated downstream. If retention has already trimmed the
  start of a session, the first event seen is not the session's first, and a
  start-of-session gap is declared that may not be real. That direction of
  error was chosen deliberately.
- Checkpoint size grows with the number of sequence domains per partition.

## Evidence

`TestGapSpanningANormalizerRestartIsStillDetected`,
`TestRedeliveredRawEventsAfterRestartAreDropped`,
`TestNormalizerCrashReprocessesWithoutDuplicatingFacts`,
`TestQuietFeedLeavesNoUnsavedProgress`, and step 2 of the failure demo
(SIGKILL of the normalizer container, store audited afterwards).
