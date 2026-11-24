"""The smallest useful strategy: buy an equal share of every symbol once, then hold.

A baseline to compare other strategies against under the same frictions.

    python -m backtester run --strategy examples/buy_and_hold.py --data data/demo/bars.parquet
"""

from backtester import EventType, Strategy


class BuyAndHold(Strategy):
    def __init__(self, allocation_pct: int = 95):
        self.allocation_pct = allocation_pct
        self._bought: set[str] = set()

    def on_market_event(self, event, context):
        symbol = event.symbol
        if event.event_type is not EventType.BAR or symbol in self._bought:
            return
        self._bought.add(symbol)
        budget = context.equity * self.allocation_pct // (100 * len(context.symbols))
        qty = budget // event.payload.close
        if qty > 0:
            context.submit_order(symbol, "buy", qty)
