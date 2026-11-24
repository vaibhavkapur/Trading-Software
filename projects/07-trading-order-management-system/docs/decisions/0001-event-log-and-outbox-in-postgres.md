# ADR 0001: One PostgreSQL transaction for the event, its projections and the outbox

Status: accepted

## Context

The spec suggests PostgreSQL plus NATS or Kafka plus Redis. The requirements that matter are:

- a repeated request must not create a second order;
- a duplicate fill must not change quantity or cash twice;
- restarting any worker must lose nothing and duplicate nothing;
- readers (the API, reconciliation) must see state that matches the history.

Each of those is a statement about several writes being atomic: the idempotency record and the order; the event and the projection; the order and the instruction to send it; the inbox record and the fill.

## Decision

Keep all of it in one PostgreSQL database and make each of those groups one transaction.

- **Event log** (`order_events`) is the source of truth. `orders`, `executions` and `positions` are projections written in the same transaction as the event that changes them.
- **Outbox** rows are written in that transaction too. Workers lease them with `FOR UPDATE SKIP LOCKED` and a lease expiry; there is no message bus.
- **Idempotency keys** and the **inbox** of broker event ids are tables with unique constraints, claimed in the same transaction as the work they guard. There is no Redis.

## Consequences

Good:

- The hard guarantees are enforced by the database, not by coordination between systems. There is no dual write anywhere, so there is no "saved but not published" or "published but not saved" state to reason about or repair.
- Projections cannot lag the log. A read after a write sees it. `omsd verify` can prove log and projections agree at any moment because both are in one consistent snapshot.
- One thing to run, back up and restore.

Costs, measured rather than assumed (see `docs/test-report.md`):

- **Every step is a durable commit.** An order that fills in two executions costs six: create, dispatch intent, dispatch outcome, acknowledgement, two fills. With commits truly flushed to storage on the test machine (about 7 ms each), an unloaded order goes from accepted to fully filled in about 23 ms at the median.
- **A hot position row serialises fills.** Every fill for one account and symbol updates the same `positions` row inside its transaction, so those fills commit one at a time. In the benchmark, with identical settings, one account and symbol completed about 104 orders per second end to end and the same load spread over eight positions completed about 483. An account that trades one symbol very heavily is the worst case.
- **The outbox is polled.** Dispatch latency is bounded below by a database round trip, and throughput by the worker's batch size and concurrency. In-process nudges remove the polling delay when the API and worker share a process; separate processes wait for the poll interval (250 ms by default).
- **Other consumers cannot subscribe.** Nothing outside this service can follow order events without querying the database.

## Alternatives considered

- **Kafka or NATS as the log, projections built by consumers.** Scales further and lets other systems subscribe. But the command path then needs either a transactional outbox anyway (to publish what it committed) or acceptance that a command can succeed and its event be lost. Projections become eventually consistent, so "create then read" and reconciliation both have to handle lag. That is machinery to solve problems this size of system does not have.
- **Asynchronous position projection** (remove the hot row from the fill transaction). Removes the contention above, at the price of positions that can be behind the fills they summarise, and a projector with its own restart and replay story. This is the first thing to change if single-symbol throughput matters; because positions are derived purely from execution deltas, they can be rebuilt from the log at any time.
- **Redis for idempotency keys.** Faster lookups, but the key and the order would no longer commit together: a crash between the two either loses the key (duplicate order on retry) or loses the order (key points at nothing).

## When to revisit

- Sustained load on one account and symbol approaches the hot-row limit: move positions to an asynchronous projection.
- Another service needs the event stream: publish from the outbox to a bus (the outbox already gives at-least-once, ordered-per-order delivery).
- Order volume makes a single primary the bottleneck: partition by account.
