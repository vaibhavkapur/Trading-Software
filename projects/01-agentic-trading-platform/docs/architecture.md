# Architecture

Paper trading only. One Python process (a modular monolith) hosts five logical services
that talk only through the interfaces wired in `src/atp/service.py`.

```text
 Browser UI / curl
        |  Bearer API key -> user id
        v
 agent-api (api.py, service.py) ------------------------------+
        |  text                                               |
        v                                                     |
 planner (planner.py)        untrusted ParsedIntent           |
   Claude or rules  --------------------------+               |
                                              v               |
 tool-gateway (tools.py)  <---- deterministic validation + sizing
   read-only: quotes, balances,               |               |
   positions, open orders                     v               |
        ^                              TradeProposal + hash   |
        |                                     |               |
        +------ policy-service (policy.py) <--+  stage: pre_approval
                 pure function -> RiskDecision                |
                                              |               |
                                   human approve / reject     |
                                   approvals.py: HMAC-signed, |
                                   expiring, bound to hash    |
                                              |               |
                                              v               |
 oms (oms.py): verify approval -> policy again (pre_submit)   |
        |      -> idempotent dispatch -> ingest fills         |
        v                                                     v
 paper broker (broker/paper.py)              audit-service (audit.py)
 simulated fills, own books                  append-only hash chain
```

## Trust boundaries

| Boundary | What crosses it | What enforces it |
|---|---|---|
| User -> API | Free text, decisions | Bearer key maps to a user id; every lookup is scoped to that user |
| Planner -> pipeline | A typed `ParsedIntent` only | `TradingService._validate_parsed` re-validates every field; the planner never sees the broker and has no tools that write |
| Pipeline -> approval | A proposal and its hash | The approver must present the hash of the proposal on file; the server recomputes it from stored parameters |
| Approval -> execution | A signed approval | `OrderManager.submit` is the only caller of `broker.submit_order`; it verifies signature, expiry, hash, and reruns policy first |
| OMS -> broker | Canonical order request | Built from the hash-verified proposal, keyed by `client_order_id` |

The tool gateway holds a `ReadOnlyBroker`, which structurally has no submit or cancel
method. Only the OMS holds the write-capable handle.

## Invariants and where they are tested

| Invariant | Mechanism | Tests |
|---|---|---|
| An unapproved order cannot execute | Orders are created only inside `decide(approve)`; FK + signature check at submit | `tests/scenarios/test_definition_of_done.py` (section 1) |
| A changed order cannot execute | Approval binds `sha256` of all order parameters; recomputed at submit; order row cross-checked | `test_order_changed_after_approval_never_executes` (9 tamper cases) |
| An approval cannot be forged or extended | HMAC over approval fields with a process-held secret | `test_forged_approval_row_is_rejected_at_submit`, `tests/unit/test_approvals_and_audit.py` |
| Duplicate submits are one broker order | Order state machine + broker idempotency on `client_order_id` + reconcile-before-resend | section 3 of the scenario file, incl. 24 concurrent submits |
| Stale quote / expired approval / broker down reject safely | Policy fails closed on missing inputs; approvals expire; ambiguous dispatch stays `submitting` | section 4 of the scenario file |
| The audit log cannot be silently edited | SQLite triggers block UPDATE/DELETE; each event hashes the previous one | `test_audit_chain_*` |
| Runs are reproducible | Injected clock, seeded price function, injected id factory | `test_replay_is_deterministic` |

## State machines

Proposal: `draft -> risk_passed | risk_rejected -> approved | rejected -> executed | failed`.
A proposal can be re-evaluated until a decision is recorded; there is exactly one decision.

Order: `created -> submitting -> accepted -> partially_filled -> filled`, with `rejected`
and `canceled` as the other terminal states. `submitting` doubles as "outcome unknown":
see [ADR 0001](decisions/0001-idempotent-dispatch-under-ambiguous-failure.md).

## Data and time

- Money and prices are `Decimal`, stored as text, serialised as JSON strings. Quantities
  are whole shares. No floats in the execution path.
- All timestamps are UTC. Quotes and fills carry venue time (`event_time`, `venue_time`)
  separately from local receive time (`received_at`). Staleness and clock-skew checks use
  venue time against the local clock.
- Risk decisions record `policy_version` and the full list of checks, not only failures.

## Deliberate simplifications

- SQLite and a process-wide lock instead of PostgreSQL and Redis. Uniqueness constraints
  (one approval and one order per proposal, unique `client_order_id` at the broker) carry
  the idempotency guarantees, so they survive a move to a multi-process store.
- The Claude planner makes one structured-output request. It does not run a tool loop.
- Trading hours are NYSE regular hours without a holiday calendar.
- API keys are static bearer tokens from the environment.
