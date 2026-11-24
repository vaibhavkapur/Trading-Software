"""Mean reversion with resting limit orders. An example, not a recommendation.

When a symbol closes more than ``entry_bps`` below its ``window``-bar average, rest a buy
limit ``discount_bps`` under that close. An entry that has not filled within
``patience`` bars is canceled. Once long, rest a sell limit at the average; if the
position has been held for ``max_hold`` bars, give up and exit at market.

This exercises limit orders, partial fills (run it with --participation), cancels and
the order-update callback.

    python -m backtester run --strategy examples/mean_reversion.py \
        --data data/demo/bars.parquet --participation 0.001 --fee-bps 1
"""

from backtester import EventType, OrderStatus, Strategy


class MeanReversion(Strategy):
    def __init__(
        self,
        window: int = 20,
        entry_bps: int = 300,
        discount_bps: int = 50,
        patience: int = 3,
        max_hold: int = 15,
        allocation_pct: int = 30,
    ):
        self.window = window
        self.entry_bps = entry_bps
        self.discount_bps = discount_bps
        self.patience = patience
        self.max_hold = max_hold
        self.allocation_pct = allocation_pct
        self._entry_age: dict[str, int] = {}  # order id -> bars since it was sent
        self._held_for: dict[str, int] = {}  # symbol -> bars since the position opened

    def on_market_event(self, event, context):
        if event.event_type is not EventType.BAR:
            return
        symbol = event.symbol
        bars = context.history(symbol, self.window)
        if len(bars) < self.window:
            return
        average = sum(bar.close for bar in bars) // len(bars)
        close = bars[-1].close
        held = context.position(symbol)
        working = context.open_orders(symbol)

        for order in working:
            if order.id in self._entry_age:
                self._entry_age[order.id] += 1
                if self._entry_age[order.id] > self.patience:
                    context.cancel_order(order.id)

        if held > 0:
            self._held_for[symbol] = self._held_for.get(symbol, 0) + 1
            exits = [o for o in working if o.side == "sell"]
            if self._held_for[symbol] > self.max_hold:
                targets = [o for o in exits if o.type == "limit"]
                for order in targets:
                    context.cancel_order(order.id)
                # Quantity still committed to an open exit cannot be sold a second time;
                # it follows on a later bar, once the cancels above are confirmed.
                free = held - sum(o.remaining for o in exits)
                if free > 0:
                    context.submit_order(symbol, "sell", free, tag="timeout")
            elif not exits:
                context.submit_order(
                    symbol, "sell", held, "limit", limit_px=max(average, 1), tag="target"
                )
            return

        self._held_for.pop(symbol, None)
        cheap = (average - close) * 10_000 > self.entry_bps * average
        if cheap and not working:
            limit = close - close * self.discount_bps // 10_000
            qty = context.equity * self.allocation_pct // 100 // limit if limit > 0 else 0
            if qty > 0:
                order_id = context.submit_order(symbol, "buy", qty, "limit", limit, tag="entry")
                self._entry_age[order_id] = 0

    def on_order_update(self, order, context):
        if order.status in (OrderStatus.CANCELED, OrderStatus.REJECTED):
            self._entry_age.pop(order.id, None)

    def on_fill(self, fill, context):
        if context.order(fill.order_id).status is OrderStatus.FILLED:
            self._entry_age.pop(fill.order_id, None)
