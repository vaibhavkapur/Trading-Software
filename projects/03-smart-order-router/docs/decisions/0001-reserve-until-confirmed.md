# ADR 0001: Reserve child quantity until the venue confirms the outcome

Status: accepted

## Context

The definition of done says no child fill may cause executed quantity to exceed the parent quantity. The hard case
is not the happy path — it is the child whose report never arrives. The venue may have filled it, partly filled
it, rejected it, or never received it. The router must decide whether to re-route that quantity.

## Options

1. **Release on timeout, re-route immediately.** Best fill rate and simplest code. If the original order did
   fill, the parent overfills — an unwanted position that someone has to trade out of.
2. **Send a cancel, release when the cancel is acknowledged.** Standard for resting orders. For IOC orders there
   is nothing to cancel, and during a link outage the cancel cannot be delivered either.
3. **Keep the quantity reserved until a terminal report is confirmed**, by a late report or by an order-status
   query after reconnect. Never overfills. Costs fill rate: quantity can stay locked for the length of an outage.

## Decision

Option 3. `oms.Book` reserves a child's full quantity at creation (`parent.reserved_qty`), and only `ApplyReport`
releases it. `Timeout` moves the child to `TIMED_OUT` without touching the reservation. The engine routes only
`target - filled - reserved`. When a venue's link returns, the engine queries the venue for the result of every
unresolved child and applies it through the same idempotent path.

A parent may reach its deadline with quantity still unresolved. It then finishes `EXPIRED`, and the report shows
`unresolved_qty` separately from `unfilled_qty`. A late fill is still applied after expiry and cannot exceed the
target, because the reservation was never released.

## Consequences

- Underfill is possible where option 1 would have filled. Under-execution is visible and recoverable;
  over-execution is a risk event. The trade is deliberate.
- A venue that never comes back leaves quantity locked for the life of the parent. Releasing it needs a human
  decision (not implemented: a manual "write off child" operation).
- Reports must be idempotent, since reconciliation can deliver a result the late report also delivers. Fill ids
  are de-duplicated per venue and reports for terminal children are ignored.
- A report that cannot be reconciled (it would overfill) engages the kill switch rather than being clamped.

## Evidence

`src/oms/oms_test.go` (`TestTimeoutKeepsReservationUntilLateReport`, `TestOverfillReportRefusedAtomically`),
`src/engine/engine_test.go` (`TestLostReportIsReservedThenReconciled`), and
`tests/scenarios/scenarios_test.go` (`TestRandomFaultTimelinesKeepInvariants`).
