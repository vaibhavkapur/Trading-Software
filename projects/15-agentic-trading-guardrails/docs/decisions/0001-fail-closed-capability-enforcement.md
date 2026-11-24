# ADR-0001: Fail-Closed Policy Re-Evaluation Combined with One-Use Proposal-Bound Capabilities

- **Status:** Accepted
- **Date:** 2026-11-24

## Context

In agentic trading systems, an AI agent typically plans an order (`order:propose`), waits for an out-of-band human or automated risk approval, and later executes the order (`order:submit`). Two common shortcuts weaken security:

1. **Planning-time-only enforcement:** Checking risk limits only when proposing, then trusting the subsequent approval token to execute without re-evaluating live policy or market freshness.
2. **Unsigned or reusable approvals:** Storing an `approved=true` boolean flag on a mutable proposal row, allowing TOCTOU tampering of order terms or duplicate/replayed execution attempts.

## Decision

We require **both** mechanisms on every broker write through a single Policy Enforcement Point (PEP):

1. **Signed, one-use, proposal-bound capability (`src/pkg/approval`):**
   - Proposals carry a canonical SHA-256 `terms_hash` covering every immutable order term (`id`, `agent_id`, `principal_id`, `account_id`, `symbol`, `side`, `order_type`, `quantity`, `limit_price_ticks`, `notional_cents`, `time_in_force`).
   - Principals mint short-lived Ed25519-signed capability tokens bound to `(proposal_id, proposal_hash, account_id, principal_id, nonce, exp)`.
   - Consumption is atomic under a mutex and durably persisted (`state.json`) so concurrent races and post-restart replays fail closed.
2. **Mandatory live deterministic re-evaluation (`src/pkg/policy` + `src/pkg/gateway`):**
   - At execution time, the PEP recomputes the canonical proposal hash from current state, checks broker connectivity and quote freshness, and re-runs the active policy bundle (`ScopeOrderSubmit`) against live daily notional, trading windows, symbol rules, and the kill switch.
   - Human approval **never** overrides a deterministic `DENY` rule or kill switch.
3. **Code-level broker isolation (`src/pkg/broker`):**
   - `PaperAdapter.SubmitOrder` requires an opaque sealed `ExecutionPermit` issued exclusively by the PEP.

## Trade-offs

- **Cons:**
  - Adds ~0.5 ms of synchronous verification overhead per write (Ed25519 verification + policy trace + hash-chained audit write).
  - If a valid approval is consumed immediately before a policy kill-switch fires or a downstream broker rejects the order, the agent must request a fresh proposal/approval rather than reusing the old capability.
- **Pros:**
  - Eliminates TOCTOU proposal mutation, replay races, cross-account confusion, and stale approvals surviving an emergency kill switch.
  - Produces a complete, tamper-evident cryptographic audit chain for every allowed and denied write.
