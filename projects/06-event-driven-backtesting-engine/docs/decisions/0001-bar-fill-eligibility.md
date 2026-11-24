# ADR 0001: What a bar is allowed to fill

Status: accepted

## Context

A bar says that between its open and close the price started at O, reached H and L in
some unknown order, and ended at C, with V units traded. It does not say when within the
bar any of that happened. A backtest has to decide, from that alone, whether and at what
price a simulated order would have traded. Each convenient answer hides a form of
look-ahead or optimism:

- **Filling at the close of the bar the strategy just saw.** The strategy decided using
  that close, so trading at it means acting on a price before knowing it.
- **Filling an order against a bar that was already under way when the order arrived.**
  The bar's open, and possibly its high and low, happened before the order existed. With
  latency this is the usual case, not an edge case.
- **Filling a limit order whenever the bar touches its price.** A real resting order is
  somewhere in a queue. If the market only touched the price, orders ahead of it may have
  taken all the volume.
- **Filling any size.** A bar with 1,000 units of volume cannot fill a 50,000 unit order.

## Decision

1. A bar can fill only orders that reached the exchange at or before the bar's open.
   An order that arrives mid-bar waits for the next bar.
2. A market order fills at that bar's open, moved against the trader by half the assumed
   spread plus slippage.
3. A limit order that is marketable at the open fills at the open with the same
   adjustment, and never at a price worse than its limit. It is a taker.
4. Otherwise a limit order fills at its limit price, as a maker with no spread or
   slippage, only if the bar trades **through** the limit (low below a buy limit, high
   above a sell limit).
5. A bar that only **touches** the limit fills it with probability
   `touch_fill_probability`, drawn from a seeded stream. The default is 0.
6. Fills are booked at the bar's close, the first moment the bar is a complete fact, and
   are reported to the strategy before the bar itself is delivered.
7. If `participation_rate` is set, the total quantity filled from one bar is at most that
   share of its volume, handed out in order of arrival. The remainder keeps working.
8. Every rounding in price or fee goes against the trader.

Each market event is processed at venue time by the exchange and at availability time by
the strategy, so rule 1 holds even when data is published late.

## Consequences

- A strategy that reacts to a daily close trades at the next day's open at the earliest.
  With intraday bars and any latency, it trades one bar later than a naive engine would.
  Results are less flattering and closer to what could have been executed.
- Booking at the close while pricing at the open means a fill is dated later than the
  price it got. This is deliberate: it keeps the ledger free of anything the simulation
  could not have known at that timestamp. Execution shortfall is measured against the
  price at the time of the decision, so the delay shows up there.
- Limit orders fill less often than in engines that fill on touch. Setting
  `touch_fill_probability` above 0 relaxes this and makes results depend on the seed.
- With no participation cap, liquidity is unlimited and capacity is overstated. The
  report says so in its assumptions.
- The model has no order book, no queue position and no market impact. Simulated orders
  never move prices. For strategies that take a meaningful share of volume, the results
  are an upper bound even with the cap on.
- Intrabar stop or bracket logic cannot be simulated honestly from bars, so the engine
  offers no stop orders. A strategy can approximate one with a market order on the next
  bar, and pays the gap.
- Quote data follows different rules (an arriving order trades against the prevailing
  quote), described in `exchange.py`. A symbol uses bars or quotes, never both.

Any change to these rules changes fills. `FILL_MODEL_VERSION` is part of every config
hash, so results made under different rules never share a run id, and the golden replay
test fails until its expectations are regenerated on purpose.

Tests: `tests/unit/test_exchange.py` (one test per rule),
`test_latency_makes_the_order_miss_the_bar_that_has_already_opened`,
`test_a_late_bar_is_seen_late_and_cannot_be_traded_retroactively`.
