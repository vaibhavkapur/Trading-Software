# ADR 0001: Point-in-time benchmarks: flag, do not backfill

Status: accepted

## Context

Arrival-price shortfall compares each fill to the market mid at the moment the order
arrived. Market data is never complete: feeds start late, drop out, deliver late, and carry
venue timestamps that disagree with local clocks. Every TCA system therefore has to decide
what to do with an order whose arrival quote is missing or doubtful.

The choice matters more than it looks. The orders most likely to arrive during a data gap
are those sent in fast or disorderly markets, which are also the expensive ones. Whatever
is done with them shifts the headline number.

## Options considered

1. **Backfill from the next available quote.** Simple, and every order gets a number. But
   the next quote is after arrival, so it already contains the price move the order may
   have caused or chased. A buy that pushed the price up is then measured against the
   higher price and looks cheap. The error is biased in the flattering direction.
2. **Carry the last quote forward without limit.** No look-ahead, but a quote from a
   minute before an outage is presented as if it were the market at arrival.
3. **Interpolate between the quotes either side of the gap.** Uses future data, like
   option 1, and invents a price nobody quoted.
4. **Drop affected orders silently.** Keeps the remaining numbers clean but hides how much
   was excluded, and the excluded orders are not a random sample.
5. **Compute strictly point-in-time, label quality, and aggregate only what is trusted,
   while reporting what was left out.**

## Decision

Option 5.

- Benchmarks use only quotes stamped at or before the instant. Fill-time quotes are taken
  strictly before the fill.
- No quote before the instant: the benchmark has no value and the order is flagged
  `ARRIVAL_QUOTE_MISSING`. Nothing is substituted.
- A quote older than `max_quote_age_ms`, a crossed consolidated quote, or a quote whose
  event and receive times disagree by more than `max_clock_skew_ms`: the value is computed
  and shown, marked `degraded`, and excluded from aggregates by default.
- Aggregates report `orders_filled`, `orders_trusted`, `orders_costed` and
  `trusted_share` on every row. `include_untrusted=true` adds the degraded orders back for
  comparison; orders with no value can never be added.
- The thresholds and the quote clock are part of the methodology id, so changing them
  changes the version stamped on every output.

## Consequences

- Headline figures cover fewer orders than were traded. On the simulated fixture, 50 of 57
  filled orders have a trusted arrival benchmark. The dashboard shows that share next to
  the cost so the two are read together.
- Excluding untrusted orders is itself a selection effect. It is visible rather than
  hidden, and `include_untrusted` shows how far the figure moves (4.4965 against 4.4413
  bps on the fixture), but it is not corrected for.
- Late-delivered quotes legitimately change results: once the missing quotes are imported
  the order becomes trusted and the dataset version changes. Reports are reproducible for a
  given dataset version, not for a given point in wall-clock time.
- Analysts cannot get a number for an order with no prior quote. That is intended; the
  per-order view explains why.

## Evidence

- `tests/unit/test_benchmark.py`: a quote one microsecond in the future is invisible;
  venues age out; all-stale returns the newest quote, marked.
- `tests/scenarios/test_definition_of_done.py`, section 2: missing arrival quotes are
  flagged, later quotes never change the result, and the order is left out of aggregates
  with `slippage_bps` null rather than zero.
- `tests/scenarios/test_data_quality.py`: an outage is flagged and heals when the gap is
  filled; staleness changes trust, not the number.
