# M3 — Integration and Validation Report

## Scope Completed
- Implemented second approved sandbox adapter `sandbox-beta` (`src/adapters/sandbox_beta/adapter.go`) with meaningfully distinct semantics:
  - Async queued acknowledgement (`PENDING_NEW` -> queue settle -> `ACCEPTED`/`FILLED`)
  - HMAC-SHA256 request signing via `SecretManager`
  - Distinct symbol conventions (`<TICKER>.US` equities/ETFs, `<BASE>-USD` spot crypto) and fractional quantity support
  - Async cancel timeout behavior (`CANCEL_PENDING` / `CANCEL_TIMEOUT`)
  - Cursor-based event replay (`ReplayEvents`) persisted in `Store`
- Implemented durable append-only JSONL write-ahead `JournalStore` (`src/storage/journal_store.go`) supporting crash recovery of orders, fills, webhook dedup keys, reconciliation reports, and replay cursors.
- Implemented pure-Go HTTP REST API and RFC 6455 WebSocket event broadcaster (`src/api/`).
- Documented idempotency, monotonic fill ordering, and reconcile-first semantics in [`docs/adr/ADR-002-idempotency-and-unknown-outcome-reconciliation.md`](../docs/adr/ADR-002-idempotency-and-unknown-outcome-reconciliation.md).

## Verification Evidence
- `TestRESTAPIAndWebSocketEventStream`: Verified REST order lifecycle, RFC 6455 WebSocket `FILL_RECORDED` & `ORDER_UPDATED` streaming, and operator halt/resume (`503` while halted).
- `TestJournalStoreCrashRecoveryAndIdempotencyPersistence`: Verified full state recovery and zero-dispatch idempotency across restart.
- `TestScenario_AsyncReplayAndFailClosedDrill`: Verified `sandbox-beta` async queue -> cursor replay -> idempotent second replay.
- Raw evidence retained in [`reports/evidence/test_suite_output.txt`](evidence/test_suite_output.txt) and [`reports/evidence/demo_output.json`](evidence/demo_output.json).
