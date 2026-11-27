# ADR-002: Idempotency, Monotonic Fill Normalization, and Reconcile-First Unknown Outcomes

- **Status:** Accepted
- **Date:** 2026-11-27
- **Context:** Network timeouts during order submit or cancel create ambiguous execution outcomes; naive retries risk double-fills and unintended market exposure. Webhooks can arrive duplicated or out of sequence.
- **Decision:**
  1. Require a unique `client_order_id` and compute a deterministic SHA-256 `payload_hash` on every `OrderRequest`. Replays with identical hash return the stored order with zero broker dispatches; mismatched hashes return `IDEMPOTENCY_CONFLICT`.
  2. When submit returns `UNKNOWN_OUTCOME` (or context timeout), transition the order to `SUBMIT_UNKNOWN` and **never blindly retry** — even if the caller replays the request. Resolution happens solely through `reconciler.ReconcileBroker` querying broker truth.
  3. Deduplicate webhooks by `(broker_id, event_id)` and fills by `(broker_id, broker_fill_id)`. Sort fills by `(sequence, executed_at_utc, broker_fill_id)` and recompute `filled_quantity`, `remaining_qty`, `avg_fill_price` (VWAP), and `total_fees` monotonically so out-of-order fills and stale status events never corrupt or regress terminal states.
- **Consequences:** Eliminates double-execution risk under transport partitions and guarantees identical end-state regardless of webhook arrival order or duplication.
