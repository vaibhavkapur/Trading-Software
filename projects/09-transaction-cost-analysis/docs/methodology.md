# Methodology (`tca-1.0`)

How every number is calculated, what it is measured against, and when it is not trusted.
Reports carry a methodology id such as `tca-1.0+a9b35f3f`: the version below plus a hash of
the parameters in force. Two reports are comparable only when both the methodology id and
the dataset version match.

All data in this repository is simulated. Nothing here measures a real broker or venue.

## Sign convention

A cost is **positive when the execution was worse than the benchmark**, for buys and sells
alike:

```text
cost = side_sign x (execution price - benchmark price) x quantity
side_sign = +1 for a buy, -1 for a sell
```

A buy that pays above the benchmark and a sell that receives below it both cost a positive
amount. Costs are computed in currency first. Basis points are always that currency amount
over the **benchmark notional** (filled quantity x benchmark price):

```text
bps = cost / (filled_qty x benchmark price) x 10,000
```

Because every figure for an order shares one denominator, the parts add up exactly:
spread + drift = arrival shortfall, and shortfall + fees = all-in cost.

## Timestamps and clocks

- All timestamps are UTC, stored as integer microseconds. Inputs without a UTC offset are
  rejected, not assumed.
- A quote has an event time `ts` (stamped by the venue) and optionally a local receive
  time `recv_ts`. The quote clock is a methodology parameter: `event` (default) aligns
  quotes to orders by `ts`; `receive` aligns by `recv_ts` where present, which shows only
  what had actually reached this system.
- If `ts` and `recv_ts` of a quote used in a benchmark differ by more than
  `max_clock_skew_ms` (default 250), the benchmark is flagged `*_CLOCK_SKEW` and not
  trusted: the two clocks disagree, so neither alignment is reliable.

## Benchmarks

### Consolidated quote as of an instant

For each venue take its most recent quote at or before the instant. Discard venues whose
quote is older than `max_quote_age_ms` (default 5,000). The best bid is the highest
remaining bid, the best ask the lowest remaining ask, and the mid is halfway between.

- **Never look ahead.** A quote stamped one microsecond after the instant is invisible. If
  no quote exists at or before it, the benchmark is `missing` and has no value. It is never
  filled in from a later quote.
- If every venue's quote is too old, the single newest quote is used, the value is shown,
  and the benchmark is `degraded` with `*_QUOTE_STALE`.
- If the best bid is above the best ask the benchmark is `degraded` with `*_QUOTE_CROSSED`.
  A locked market (bid equals ask) is not flagged.
- For fills the lookup is **strictly before** the fill time, because a quote carrying the
  same timestamp as a trade may already reflect that trade.

### `arrival_mid` and `decision_mid`

The consolidated mid at `arrival_ts` (when the order reached the market) and at
`decision_ts` (when the decision to trade was made). Arrival is the headline benchmark;
the difference between the two is the cost of the delay between deciding and arriving.

### `interval_vwap`

The volume-weighted price of every trade bar touching `[arrival_ts, last fill]`:

```text
interval_vwap = sum(bar.vwap x bar.volume) / sum(bar.volume)
```

Whole bars are used, so the window is rounded outward to bar boundaries and includes some
trading from just before arrival and just after the last fill. The exact window is
recorded with the benchmark.

- Bars missing inside the window: `VWAP_BARS_PARTIAL`, value shown, not trusted.
- Order shorter than one bar: `VWAP_BARS_COARSE`, value shown, not trusted. The bar mostly
  reflects trading outside the order's life.
- No bar volume in the window: `VWAP_BARS_MISSING`, no value.

The bars include the order's own fills where the data source includes them, so interval
VWAP is not independent of the order being measured.

## Measures per order

| Measure | Definition |
|---|---|
| Arrival shortfall | `sign x (executed notional - filled_qty x arrival_mid)` |
| Decision shortfall, VWAP deviation | the same against `decision_mid` and `interval_vwap` |
| Spread paid | sum over fills of `sign x qty x (fill price - mid just before the fill)` |
| Drift | sum over fills of `sign x qty x (mid just before the fill - arrival_mid)` |
| Explicit fees | sum of `fee` over fills; a negative fee is a rebate |
| All-in cost | arrival shortfall + fees |
| Effective spread | `2 x spread paid / sum(qty x mid just before the fill) x 10,000` bps |
| Quoted spread at arrival | `(ask - bid) / mid x 10,000` bps |
| Fill rate | `filled_qty / order qty` |
| Time to first / last fill | fill time minus `arrival_ts` |
| Participation | `filled_qty / bar volume in the VWAP window` |

**Drift is not market impact.** It is how far the mid moved between arrival and each fill.
That movement is the order's own impact plus whatever the market would have done anyway,
and this data cannot separate the two. The dashboard labels it accordingly.

The spread/drift split is only computed when the arrival benchmark exists and every fill
has a prior quote. It is only *trusted* when the arrival benchmark is trusted and every
one of those fill quotes is `ok`.

## Trust

A comparison of an order against a benchmark is **trusted** when all of these hold:

1. the benchmark status is `ok` (not `degraded`, not `missing`);
2. the order has fills;
3. the order's own records are consistent: not `OVERFILLED`, no `FILL_BEFORE_ARRIVAL`.

Trust is per benchmark. An order can have a trusted arrival shortfall and an untrusted
VWAP deviation.

### Flags

| Flag | Meaning | Effect |
|---|---|---|
| `NO_FILLS` | no executions | no cost measures; counts toward fill rate |
| `PARTIAL_FILL` | filled less than ordered | costs cover the executed quantity only; the opportunity cost of the remainder is not estimated |
| `OVERFILLED` | fills exceed the order quantity | no benchmark trusted for this order |
| `FILL_BEFORE_ARRIVAL` | a fill is stamped before the order arrived | no benchmark trusted for this order |
| `ARRIVAL_QUOTE_MISSING`, `DECISION_QUOTE_MISSING` | no quote at or before the instant | benchmark has no value |
| `ARRIVAL_QUOTE_STALE`, `DECISION_QUOTE_STALE` | newest quote older than the limit | value shown, not trusted |
| `ARRIVAL_QUOTE_CROSSED`, `DECISION_QUOTE_CROSSED` | consolidated bid above ask | value shown, not trusted |
| `ARRIVAL_CLOCK_SKEW`, `DECISION_CLOCK_SKEW` | event and receive time too far apart | value shown, not trusted |
| `FILL_QUOTE_MISSING` | a fill has no prior quote | spread/drift split not computed |
| `FILL_QUOTE_STALE`, `FILL_QUOTE_CROSSED`, `FILL_CLOCK_SKEW` | a fill matched a degraded quote | split computed, low confidence |
| `VWAP_BARS_MISSING`, `VWAP_BARS_PARTIAL`, `VWAP_BARS_COARSE`, `VWAP_WINDOW_INVALID` | see above | VWAP missing or not trusted |

Each flag comes with a plain-language note on the order, for example *"The newest quote at
arrival was 41.2s old (limit 5s). The arrival benchmark is shown but excluded from trusted
aggregates."*

## Aggregation

- Group figures are **ratios of sums**: total cost over total benchmark notional. They are
  not averages of per-order basis points, so a 100-share order cannot outweigh a
  100,000-share one.
- Cost figures include **only orders whose benchmark is trusted**, unless
  `include_untrusted` is set. Every row reports `orders_filled`, `orders_trusted`,
  `orders_costed` and `trusted_share` so the excluded part is visible. Orders with no
  benchmark value are never costed, whatever the setting.
- Fill rate and fees cover all orders in the group; they need no benchmark.
- In a group, `spread + drift + unsplit = slippage`. `unsplit` is the slippage of costed
  orders whose fills could not all be matched to a trusted quote.
- **Venue rows** hold only the fills at that venue, measured against the parent order's
  benchmark. Their fill rate is filled over routed child quantity.
- Buys and sells are never netted by sign: both are already "positive = worse".
- **Rows are not normalised for order difficulty.** A venue, broker or strategy that
  receives larger, more urgent or wider-spread orders will look worse without executing
  worse. Every grouped response says so. Do not rank on these figures alone.

## Rounding and reproducibility

All arithmetic is `Decimal`; nothing passes through binary floating point. Values are
rounded only when written out, half-even: 4 decimals for currency and basis points, 6 for
prices and ratios, 3 for milliseconds.

A report bundle contains no wall-clock time. The same inputs under the same methodology
produce byte-identical files; the manifest records the methodology and its parameters, the
content hash of each input dataset, the options used, and the SHA-256 of each output file.

## Worked example

The input files for this example are in `examples/worked/`.

Market for XYZ (two venues):

| Time (UTC) | V1 bid / ask | V2 bid / ask | Best bid / ask | Mid |
|---|---|---|---|---|
| 14:59:57 | 99.98 / 100.02 | | 99.98 / 100.02 | 100.00 |
| 14:59:59 | 100.00 / 100.04 | 99.99 / 100.05 | 100.00 / 100.04 | 100.02 |
| 15:00:04 | 100.01 / 100.05 | 100.00 / 100.06 | 100.01 / 100.05 | 100.03 |
| 15:01:19 | 100.06 / 100.10 | 100.05 / 100.11 | 100.06 / 100.10 | 100.08 |

Bars: 15:00 VWAP 100.04 on 12,000 shares; 15:01 VWAP 100.09 on 8,000 shares.

**W-BUY**: buy 1,000, decided 14:59:58, arrived 15:00:00. Fills: 400 @ 100.05 at 15:00:05
(fee 1.20) and 600 @ 100.10 at 15:01:20 (fee 1.80).

```text
arrival mid   = (100.00 + 100.04) / 2                      = 100.02
decision mid  = (99.98 + 100.02) / 2                       = 100.00
average price = (400 x 100.05 + 600 x 100.10) / 1,000      = 100.08

arrival shortfall = +1 x (400 x 0.03 + 600 x 0.08)         = 12 + 48 = 60.00
                  = 60 / (1,000 x 100.02) x 10,000         = 5.9988 bps
spread paid       = 400 x (100.05 - 100.03) + 600 x (100.10 - 100.08) = 8 + 12 = 20.00
drift             = 400 x (100.03 - 100.02) + 600 x (100.08 - 100.02) = 4 + 36 = 40.00
fees              = 1.20 + 1.80                            = 3.00  (0.2999 bps)
all-in            = 60 + 3                                 = 63.00 (6.2987 bps)

decision shortfall = 1,000 x (100.08 - 100.00)             = 80.00 (8.0000 bps)
interval VWAP      = (100.04 x 12,000 + 100.09 x 8,000) / 20,000 = 100.06
VWAP deviation     = 1,000 x (100.08 - 100.06)             = 20.00 (1.9988 bps)
effective spread   = 2 x 20 / (400 x 100.03 + 600 x 100.08) x 10,000 = 3.9976 bps
participation      = 1,000 / 20,000                        = 0.05
```

**W-SELL**: sell 1,000 in the same market. Fills: 400 @ 100.01 and 600 @ 100.06, average
100.04.

```text
arrival shortfall = -1 x (400 x (100.01 - 100.02) + 600 x (100.06 - 100.02))
                  = -1 x (-4 + 24)                         = -20.00 (-1.9996 bps)
spread paid       = -1 x (400 x (100.01 - 100.03) + 600 x (100.06 - 100.08)) = +20.00
drift             = -1 x (400 x 0.01 + 600 x 0.06)         = -40.00
all-in            = -20 + 3                                = -17.00
```

The seller paid the same spread as the buyer (+20) but the market rose while it was
selling, so drift is negative and the order beat its arrival price.

Reproduce:

```bash
PYTHONPATH=src .venv/bin/python -m tca report --data examples/worked --out reports/worked
```

`reports/worked/orders.csv` shows these figures; `tests/unit/test_attribution.py` and
`tests/scenarios/test_definition_of_done.py` assert them.
