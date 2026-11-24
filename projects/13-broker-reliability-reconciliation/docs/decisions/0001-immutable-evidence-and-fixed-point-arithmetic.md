# ADR 0001: Immutable Original Evidence, Deterministic Replay IDs, and Fixed-Point Arithmetic

- **Status:** Accepted
- **Date:** 2026-11-24

## Context

Financial reconciliation systems sit on the boundary between trading operations, accounting, and audit compliance. Three engineering hazards routinely break naive reconciliation implementations:

1. **Mutable exception records:** Allowing resolutions or subsequent reruns to overwrite the original discrepancy snapshot destroys the forensic audit trail needed to explain why a break occurred.
2. **Non-deterministic reruns:** Generating random UUIDs for runs/issues on every execution makes reruns noisy, duplicates operator work queues, and breaks deterministic replay verification.
3. **Binary floating-point drift and integer overflow:** Using `float64` for price/fee/cash deltas causes false breaks (e.g., `0.1 + 0.2 != 0.3`), while naive scaled `int64` multiplication (`qty * price`) overflows on institutional notionals (`10^9 * 500 * 10^16`).

## Decision

1. **Immutable Original Evidence + Append-Only History:**
   - Every `ReconIssue` stores an `Evidence` snapshot captured at detection time.
   - `issues.Store` clones `Evidence` on read and write and forbids any API or resolution call from modifying `Evidence`.
   - Re-running reconciliation on the same discrepancy preserves the existing issue's `State`, `Owner`, `Resolution`, and original `Evidence`, appending only a `"reobserved"` audit entry when a new run ID observes it.
   - State changes, owner assignments, and resolutions append structured `IssueHistoryEntry` records to an append-only JSONL log.

2. **Deterministic Content-Addressed IDs:**
   - Inputs are normalized and canonically sorted prior to computing a SHA-256 `InputDigest`.
   - `ReconRun.ID` is derived from `SHA-256(account_id | cutoff | policy_version | input_digest)`.
   - `ReconIssue.ID` is derived from `SHA-256(account_id | cutoff | policy_version | kind | ref_key)`.
   - Replaying the same cutoff, policy, and snapshots therefore yields byte-for-byte identical classifications, digests, and IDs.

3. **Fixed-Point `Decimal` Backed by `*big.Int` at Scale $10^8$:**
   - All quantities, prices, fees, notionals, and cash balances use `domain.Decimal`, an immutable value type storing integer units scaled by $10^8$ inside `math/big.Int`.
   - `Mul` computes exact integer product followed by half-up rounding back to $10^8$, preventing both binary float drift and 64-bit overflow.
   - Conversion to `float64` is isolated strictly to `Float64ForMetrics()` for Prometheus text exposition and is prohibited in comparison/reconciliation paths.

## Consequences

- **Positive:** Audit-grade evidence retention, idempotent scheduled reruns, zero floating-point rounding false positives, and zero external dependencies.
- **Trade-offs:** `math/big.Int` allocates more heap objects per arithmetic operation than raw `int64`. For daily/intraday account reconciliation (thousands to tens of thousands of fills per run), measured latency remains in single-digit milliseconds while guaranteeing exactness and overflow safety. If ultra-high-frequency millions-of-fills streaming is later required, a hybrid `int64` fast path with `big.Int` fallback can be dropped into `src/domain/decimal.go` without changing any caller API.
