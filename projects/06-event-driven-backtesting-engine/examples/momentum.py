"""Time-series momentum on bars. An example of the strategy contract, not a recommendation.

For each symbol: hold a long position while the latest close is above the close
``lookback`` bars ago by more than ``threshold_bps``; otherwise hold nothing. Orders are
market orders, so they fill at the next bar's open with spread and slippage applied.

    python -m backtester run --strategy examples/momentum.py --data data/demo/bars.parquet \
        --param lookback=40 --slippage-bps 3 --fee-bps 1 --seed 42
"""

from backtester import EventType, Strategy


class Momentum(Strategy):
    def __init__(self, lookback: int = 40, threshold_bps: int = 0, allocation_pct: int = 90):
        if lookback < 1:
            raise ValueError("lookback must be at least 1")
        if not 0 < allocation_pct <= 100:
            raise ValueError("allocation_pct must be in (0, 100]")
        self.lookback = lookback
        self.threshold_bps = threshold_bps
        self.allocation_pct = allocation_pct

    def on_market_event(self, event, context):
        if event.event_type is not EventType.BAR:
            return
        symbol = event.symbol
        bars = context.history(symbol, self.lookback + 1)
        if len(bars) <= self.lookback or context.open_orders(symbol):
            return

        then, now = bars[0].close, bars[-1].close
        # Integer comparison of (now / then - 1) against the threshold in basis points.
        trending_up = (now - then) * 10_000 > self.threshold_bps * then
        held = context.position(symbol)

        if trending_up and held == 0:
            # An equal share of equity per symbol, never more than the cash on hand, and
            # kept below 100% so an adverse open on the fill bar does not overdraw it.
            share = context.equity * self.allocation_pct // (100 * len(context.symbols))
            budget = min(share, context.cash * self.allocation_pct // 100)
            qty = budget // now
            if qty > 0:
                context.submit_order(symbol, "buy", qty, tag="entry")
        elif not trending_up and held > 0:
            context.submit_order(symbol, "sell", held, tag="exit")
