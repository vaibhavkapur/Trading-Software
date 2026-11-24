# Runbook

Simulated / paper trading only. This is the operator's guide: what to watch, what each issue means, and how to repair it.

Operator calls need a token with the `operator` role. In the examples `$OMS` is the API base URL and `$OP` is that token, both taken from your environment, never typed into a script.

## Health

| Check | Meaning |
|---|---|
| `GET /healthz` | The process is up |
| `GET /readyz` | The database answers. 503 `NOT_READY` otherwise |
| `GET /metrics` | Prometheus text format. Not authenticated: publish the port on a trusted network only |
| `omsd verify` | Replays the whole event log and compares it with `orders`, `executions` and `positions`. Exit status 1 and a list of problems if they differ. Read-only; safe on a live system; loads the whole log into memory |

Run `omsd verify` after any restore, and on a schedule. A problem it reports means a projection was changed outside the service, or there is a bug: stop and investigate before doing anything else, because every other tool reads the projections.

## What to watch

| Metric | Healthy | When it is not |
|---|---|---|
| `oms_outbox_oldest_pending_age_seconds` | Seconds | Rising: the broker is unreachable or the outbox worker is not running. Check `oms_outbox_dispatch_total{result="retry"}` and the worker's logs |
| `oms_outbox_pending` | Near zero between bursts | Same |
| `oms_outbox_dead` | 0 | Anything else needs an operator: see `OUTBOX_DEAD_LETTER` below |
| `oms_submit_to_ack_seconds` | Stable | Rising with a healthy outbox: the broker is slow to acknowledge, or its webhooks are delayed |
| `oms_order_state_lag_seconds` | Stable | Rising: webhooks are delayed. Uses the broker's clock, so compare with the next row first |
| `oms_broker_future_timestamps_total` | Flat | Rising: the broker's clock is ahead of ours. State is unaffected; lag figures are unreliable until it is fixed |
| `oms_broker_events_total{outcome="DUPLICATE"}` over the total | Low and steady | A jump means the broker is redelivering: usually the OMS was answering webhooks slowly or with 503 |
| `oms_broker_events_total{outcome="QUARANTINED"}` | Flat | Every increment opened or updated an issue |
| `oms_webhook_rejected_total{reason="signature"}` | 0 | The webhook secret differs between the two sides, or something else is posting to the endpoint |
| `oms_reconciliation_runs_total{status="FAILED"}` | Flat | The broker snapshot is unavailable. Drift is not being detected while this continues |
| `oms_reconciliation_drift_total` by kind | `MISSING_EXECUTION` and `MISSED_REPORT` occasionally | Those two are lost webhooks, repaired automatically. Any other kind opened an issue |
| `oms_reconciliation_open_issues{severity}` | 0 | Work the queue, `CRITICAL` first |
| `oms_reconciliation_repair_seconds` | Within your target | How long issues stay open, by kind |

## The exception queue

```sh
curl -sS "$OMS/v1/reconciliation/issues?status=OPEN" -H "Authorization: Bearer $OP"
curl -sS "$OMS/v1/reconciliation/issues/$ISSUE"       -H "Authorization: Bearer $OP"
```

Each issue has a `kind`, a `severity`, the order it concerns, `details` (the report or the two versions that disagree), and `occurrences`: the same problem seen again updates the open issue instead of opening another.

To resolve one:

```sh
curl -sS -X POST "$OMS/v1/reconciliation/issues/$ISSUE/resolve" \
  -H "Authorization: Bearer $OP" -H "Content-Type: application/json" \
  -d '{"action":"ACKNOWLEDGE","note":"why this is safe to close"}'
```

A note is required. The action, your principal and the note are stored on the issue, and any change to an order is written to that order's history as an `OPERATOR` event.

| Action | Applies to | What it does |
|---|---|---|
| `ACKNOWLEDGE` | Any issue | Closes it. Changes nothing else |
| `ACCEPT_EXECUTION` | `OVERFILL` | Books the quarantined execution as an explicit, attributed correction. Filled quantity will exceed order quantity |
| `REJECT_UNSUBMITTED` | `MISSING_AT_BROKER`, `OUTBOX_DEAD_LETTER` | Rejects an order stuck in `SUBMITTING`, after the OMS itself confirms with the broker that the order is unknown there |
| `REQUEUE_OUTBOX` | `OUTBOX_DEAD_LETTER` | Gives the broker call a fresh set of attempts |

### Issue kinds

**`OVERFILL`** (critical). The broker reported a fill that would take filled quantity above order quantity. The execution is not booked; position and cash are unchanged.
1. Compare the issue's `details.report` with the broker's own record of the order.
2. If the broker confirms the trade is real: `ACCEPT_EXECUTION`. The position now includes it.
3. If the broker says it was sent in error or has busted it: `ACKNOWLEDGE`.
4. If the answer is `ALREADY_BOOKED`, the execution was booked normally after all (reordered webhooks made it look like an overfill for a moment, and reconciliation has since booked it). Check the order is consistent with the broker and `ACKNOWLEDGE`.

**`UNEXPECTED_EXECUTION`** (critical). A fill for an order in a state that cannot trade, such as one the broker itself rejected. Not booked. Establish with the broker whether the trade exists. There is no action that books it: if it is real, the broker's records and ours need a correction that starts on the broker's side.

**`CONFLICTING_REPORT`** (critical for executions, otherwise high). The broker contradicted something already on record: an execution id we already have with different quantity, price or fee; a second correction of the same execution; or a replace confirmation with terms that were not requested, or when no replace was pending. Not applied. For executions the broker should send a proper correction (a new execution id referencing the old one); once the two sides agree, `ACKNOWLEDGE`.

**`INVALID_REPORT`** (medium, critical for executions). A report whose own content is unusable, such as an execution with a non-positive quantity or price. Not applied. Usually a broker-side defect to report; `ACKNOWLEDGE`.

**`ORPHAN_BROKER_EVENT`** (critical if it is an execution). A report for an order id this OMS does not have. Either the broker is sending us someone else's traffic or the OMS database is missing data. Do not acknowledge until you know which.

**`OUTBOX_DEAD_LETTER`** (high). A broker call failed `OMS_OUTBOX_MAX_ATTEMPTS` times. The order is left as it was; for a submission that means `SUBMITTING`, because whether the broker has the order is unknown.
1. Read `details.last_error`. Fix the cause (broker outage, credentials).
2. Usually: `REQUEUE_OUTBOX`. The call is safe to repeat; the broker deduplicates.
3. If the order should not be sent any more: `REJECT_UNSUBMITTED`. It is refused with `BROKER_HAS_ORDER` if the broker has the order after all (run reconciliation instead), and with `BROKER_UNAVAILABLE` if the broker cannot be asked.

**`MISSING_AT_BROKER`** (high). The OMS delivered a submission and the broker's snapshot does not contain the order. Ask the broker. If it truly never arrived: `REJECT_UNSUBMITTED` (same safeguards as above). The issue closes itself if a later run finds the order at the broker.

**`EXECUTION_MISMATCH`** (critical). Both sides have the execution with different terms. `details` shows both. The OMS does not change its record. If the broker is right it must send a correction, which the OMS applies normally; if the broker then fixes its books, the next run closes the issue by itself.

**`PHANTOM_EXECUTION`** (critical). The OMS has booked an execution the broker's snapshot does not contain. The original webhook is in `broker_events` by event id. If the broker busted the trade, it must send the bust. Do not acknowledge on the assumption that the snapshot is merely incomplete.

**`STATE_MISMATCH`, `QUANTITY_MISMATCH`** (high). Status, quantity, limit price or filled quantity differ and no single missed report explains it. Look at the order's events (`GET /v1/orders/{id}/events`) next to the broker's record. These close themselves when the two agree again.

**`ORPHAN_BROKER_ORDER`** (critical). The broker has an order under our credentials that the OMS has no record of. Treat as an incident: something other than this OMS is placing orders, or the OMS has lost data.

**`MISSING_EXECUTION`, `MISSED_REPORT`** (medium, already resolved). Records of automatic repairs. They appear with status `RESOLVED` and resolver `reconciler`; they are there to be counted.

## Reconciliation

Scheduled every `OMS_RECONCILE_INTERVAL` by processes with the `reconciler` role. To run one now:

```sh
curl -sS -X POST "$OMS/v1/reconciliation/runs" -H "Authorization: Bearer $OP"
```

- `201` with the run: `drift_found`, `auto_repaired`, `issues_opened`, `issues_cleared`.
- `409 RUN_IN_PROGRESS`: another run holds the lock. Wait.
- `502 RECONCILIATION_FAILED`: the snapshot could not be fetched. Nothing was opened or closed. The run is recorded as `FAILED`.

Changes younger than `OMS_RECONCILE_GRACE` (5s by default) are skipped and picked up by the next run. Closed orders older than 24 hours are not compared unless they have an open issue.

## Common situations

**The broker is down.** Orders are still accepted and stored. `oms_outbox_pending` and its age rise. When the broker returns the outbox drains by itself. If the outage outlasts the retry budget, messages dead-letter: requeue them once the broker is back.

**Orders stuck in `SUBMITTING`.** Either the submission has not been delivered (outbox pending or dead) or it was delivered and the acknowledgement never arrived. Check the order's outbox state through the metrics above, then run reconciliation: if the broker has the order, the missing reports are applied.

**An order shows less filled than the broker.** A webhook was lost. The next reconciliation run books it. If it does not, there is an open issue for that order explaining why.

**Restarting or deploying.** `SIGTERM` stops the API, lets in-flight broker calls finish and record their outcome, and exits 0. `SIGKILL` or a crash is also safe: leases expire (30s by default) and the work is retried by whichever worker is running. Nothing accepted by the API is lost in either case.

**Rotating credentials.**
- API keys: change `OMS_API_KEYS` (or the file) and restart the API processes. Tokens must be at least 24 characters. They are stored only as hashes in memory.
- Broker API key and webhook secret: change both sides together. While the API key differs, submissions fail with 401 and are retried, not rejected. While the webhook secret differs, webhooks are refused with 401; the simulated broker treats that as permanent and does not resend, so those reports are lost. Run reconciliation after the rotation: it books what was missed.

**Restoring the database.** Restore, run `omsd verify`, then run reconciliation before taking orders: anything that happened at the broker after the backup shows up as drift or issues.

## What never to do

- Do not edit `orders`, `executions` or `positions` by hand. They are derived; `omsd verify` will report the difference and the event log will disagree with what readers see. `order_events` and `executions` refuse UPDATE and DELETE.
- Do not point `OMS_BROKER_URL` at anything but the simulator. `omsd` checks and refuses to start, but the check is a guard, not a licence to try.
- Do not put tokens or secrets in command lines, scripts, tickets or logs.
