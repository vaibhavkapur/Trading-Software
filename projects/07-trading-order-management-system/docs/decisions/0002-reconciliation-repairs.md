# ADR 0002: What reconciliation may repair by itself, and what it must escalate

Status: accepted

## Context

When the OMS and the broker disagree, one of them is wrong, and reconciliation cannot always tell which. The spec is explicit about one side of this ("do not mutate executed trades to hide reconciliation discrepancies") and equally explicit that discrepancies must be repaired in a measurable time. Those pull in opposite directions:

- Repair too little, and every lost webhook becomes a ticket. Operators learn to click through issues, and the one that matters is missed.
- Repair too much, and the OMS overwrites its own records with whatever the broker last said, which destroys the evidence of exactly the errors reconciliation exists to catch, and makes a broker-side bug an OMS-side fact.

## Decision

Reconciliation applies a difference automatically only when **all three** hold:

1. **The broker is ahead, not different.** The OMS is missing information; nothing it already recorded is contradicted.
2. **The normal report path would have accepted it.** The repair is expressed as the same normalized report a webhook would have produced and goes through `IngestBrokerReport`, so it passes the same state machine, the same deduplication and the same overfill check. Reconciliation has no private way to write.
3. **It is old enough.** It is outside the grace window, so the webhook has had its chance.

That covers two cases: an execution the OMS has not booked (`MISSING_EXECUTION`) and a status the OMS never heard about (`MISSED_REPORT`). Each repair is recorded as a resolved issue and as an event with source `RECONCILER`, so it is counted and auditable.

Everything else is raised for a person and nothing is changed:

| Finding | Why it is not automatic |
|---|---|
| `EXECUTION_MISMATCH` | Both sides have the trade with different terms. Adopting the broker's would rewrite a booked execution. If the broker is right it will send a correction, which is the documented way for a booked trade to change |
| `PHANTOM_EXECUTION` | The OMS booked something the broker does not show. Deleting it would hide either a broker data loss or an OMS bug |
| `STATE_MISMATCH`, `QUANTITY_MISMATCH` | The books disagree in a way no single missed report explains |
| `MISSING_AT_BROKER` | Looks like "never sent", but rejecting an order that the broker is in fact working leaves a live order nobody is tracking |
| `ORPHAN_BROKER_ORDER` | An order at the broker with no OMS record is either someone else's or a serious fault |
| Overfill found in the snapshot | Same rule as for a webhook: quarantined |

Operator repairs are narrow and each re-checks its own precondition at the moment it runs:

- `ACCEPT_EXECUTION` books a quarantined overfill as an explicit `ExecutionAccepted` event carrying the operator and the issue. It is the only way filled quantity can exceed order quantity.
- `REJECT_UNSUBMITTED` closes an order stuck in `SUBMITTING`, but only after asking the broker directly, and it is refused if the broker has the order, cannot be reached, or the submission is still being retried.
- `REQUEUE_OUTBOX` gives a dead-lettered call a fresh set of attempts.
- `ACKNOWLEDGE` closes an issue and changes nothing.

Every repair requires a note and is recorded with who, when and why.

Issues that describe a difference visible in a snapshot close themselves when a later run looks at the order and the difference is gone, for example after the broker corrects its own books.

## Consequences

- A lost webhook, the common case, heals without anyone doing anything, typically within one reconciliation interval plus the grace window.
- A real disagreement always reaches a person with both versions attached, and the OMS's record of what it believed is intact.
- The automatic path cannot do anything a webhook could not do, so it cannot overfill an order, move it backwards, or double-count.
- One awkward case is accepted. If webhooks are reordered so that a correction that lowers a fill arrives after a later fill, that later fill is briefly an overfill and is quarantined. Reconciliation then books it normally, but the original `OVERFILL` issue stays open until an operator acknowledges it (`ACCEPT_EXECUTION` answers `ALREADY_BOOKED`). An overfill report is serious enough that a human looking at it once is the right default, even when it turns out to have been transient. `TestScenario_TransientOverfillFromReorderingIsRecoveredByReconciliation` pins this behaviour.

## Alternatives considered

- **Broker is always right.** Simple and self-healing, and it converts every broker-side error into silent OMS state. Rejected by the spec's non-goals.
- **Never repair automatically.** Safe, and unusable: the exception queue would be dominated by lost webhooks.
- **Repair by writing projections directly.** Faster to build, but it would bypass the event log, so the repair would not be in the order's history and `verify` would no longer hold.
