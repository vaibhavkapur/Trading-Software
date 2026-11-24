# 0002. Prices and sizes are integer ticks and lots

Status: accepted

## Context

Providers send prices as decimal strings (`"101.25"`) or scaled integers
(`1012500` meaning 101.2500). Binary floating point cannot represent most
decimal prices exactly, and two feeds parsed through `float64` can disagree in
the last bit about the same price. The specification asks for integer ticks or
fixed-point decimals.

## Decision

Canonical prices are `int64` counts of the instrument's `tick_size`, and sizes
`int64` counts of its `lot_size`. Both units are decimal strings in reference
data and are returned with every query response.

Conversion (`src/fixed`) works on the decimal digits. A value that is not an
exact multiple of its unit, has more digits than can be represented, or
overflows `int64` is an error. Nothing is rounded, and no `float64` appears
between the wire and the store.

## Alternatives considered

- **A decimal type end to end.** Exact, but every consumer then needs a
  decimal library, and ClickHouse `Decimal` columns need one scale per column
  while instruments differ in precision.
- **One global scale (e.g. 10⁻⁸).** Simple, but loses the tick grid: an
  off-tick price is a data error worth catching, and a global scale cannot
  see it.

## Consequences

- Off-tick or off-lot input is rejected to `md.rejects`
  (`PRICE_NOT_ON_TICK`, `QUANTITY_NOT_ON_LOT`) instead of being stored
  approximately.
- Comparing prices across instruments needs their tick sizes. That is the
  consumer's arithmetic to do, with the units the API hands it.
- A tick-size change for an instrument is a reference-data change the platform
  does not version; stored integers would be re-interpreted. This is listed as
  a limitation.

## Evidence

`TestUnitsAreExact`, `TestUnitsRefuseWhatCannotBeRepresented` and
`FuzzUnitsNeverRounds` (2.1 million executions in the recorded run, checking
that formatting the result reproduces the input value exactly).
