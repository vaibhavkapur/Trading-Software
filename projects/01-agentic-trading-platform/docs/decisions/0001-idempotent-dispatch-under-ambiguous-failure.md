# ADR 0001: Idempotent dispatch under ambiguous broker failure

Status: accepted

## Context

When a submit call to a broker fails at the connection level, the caller cannot tell
whether the order was received. Two wrong responses are tempting:

- Treat it as failed and let the user retry with a new order: risks two live orders.
- Treat it as sent: risks telling the user they have an order that does not exist.

The approval makes this harder. An approval expires after `ATP_APPROVAL_TTL_SECONDS`
(default 120s). If the broker booked the order and the outage outlasts the approval, a
naive retry path would reject on expiry and orphan a live order.

## Decision

1. The order's `client_order_id` is the idempotency key, generated at approval time and
   stored before any dispatch. The broker adapter contract requires that resubmitting a
   known key returns the existing order.
2. The OMS writes `submitting` durably before calling the broker. A connection failure
   leaves the order in `submitting`, which means "outcome unknown", and the API returns
   `503` with `retryable: true`. Nothing is assumed filled.
3. Any later submit or read of a `submitting` order first asks the broker for that key.
   If the broker has it, the OMS adopts the broker's state and fills without re-checking
   the approval, because execution was already authorised when it was dispatched.
4. Only if the broker has no record does the OMS re-verify the approval and rerun the
   policy gate before sending again. An expired approval at that point is a terminal
   rejection, which is safe because the broker never had the order.
5. Within one submit call the OMS retries up to `ATP_MAX_SUBMIT_ATTEMPTS` times with the
   same key.

## Consequences

- Duplicate, concurrent and post-crash submits all converge on one broker order
  (`test_lost_ack_resolves_to_one_broker_order`,
  `test_restart_mid_dispatch_reconciles_instead_of_resending`,
  `test_lost_ack_then_expired_approval_still_reconciles`).
- An order can sit in `submitting` indefinitely if the broker stays down and nobody
  retries. There is no background reconciler yet; reads and submits trigger it. This is
  the main operational gap and is listed under Planned in the README.
- Correctness depends on the broker honouring the idempotency key. A real adapter for a
  broker without that feature would need lookup-by-client-id before every send, which
  step 3 already does, plus a bounded window in which the lookup is trusted.
- If the broker is unreachable for reads before the first dispatch, the policy gate fails
  closed (`PORTFOLIO_UNAVAILABLE`) and the order is rejected outright, not queued. We
  chose a terminal rejection over a queue so that an approval is never executed against
  a portfolio the gate could not see.
