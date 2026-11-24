"""Paper broker simulator. No real-money connectivity exists anywhere in this project.

Idempotent on `client_order_id`: resubmitting returns the existing order. Fault
injection covers a hard disconnect and a lost acknowledgement (order accepted, reply
dropped), which is the case idempotent dispatch has to survive.
"""

from __future__ import annotations

from dataclasses import dataclass
from decimal import ROUND_HALF_UP, Decimal

from ..clock import Clock
from ..db import Database
from ..domain import CENT, AccountSnapshot, Fill, OrderType, Position, Side
from ..ids import IdFactory
from ..marketdata import MarketDataUnavailable, SimulatedMarketData
from .base import BrokerOrderRequest, BrokerOrderStatus, BrokerUnavailable

OPEN_STATES = ("accepted", "partially_filled")


@dataclass
class BrokerFaults:
    disconnected: bool = False  # everything fails
    order_entry_down: bool = False  # submits fail before reaching the book; reads work
    fail_next_submits: int = 0  # transient: the next N submits fail, then recover
    drop_next_ack: bool = False  # next submit is booked but the reply is lost


class PaperBroker:
    def __init__(
        self,
        db: Database,
        market_data: SimulatedMarketData,
        clock: Clock,
        new_id: IdFactory,
        starting_cash: Decimal,
        max_fill_per_match: int | None = None,
    ) -> None:
        self._db = db
        self._md = market_data
        self._clock = clock
        self._new_id = new_id
        self._starting_cash = starting_cash
        self._max_fill = max_fill_per_match
        self.faults = BrokerFaults()

    # -- connectivity -------------------------------------------------------
    def _check_connection(self) -> None:
        if self.faults.disconnected:
            raise BrokerUnavailable("paper broker disconnected")

    # -- reads --------------------------------------------------------------
    def _ensure_account(self, user_id: str) -> None:
        self._db.insert(
            "broker_accounts",
            {"user_id": user_id, "cash": str(self._starting_cash)},
            or_ignore=True,
        )

    def get_account(self, user_id: str) -> AccountSnapshot:
        self._check_connection()
        with self._db.transaction():
            self._ensure_account(user_id)
            cash = self._db.one("SELECT cash FROM broker_accounts WHERE user_id = ?", (user_id,))
            rows = self._db.all(
                "SELECT symbol, quantity, avg_cost FROM broker_positions "
                "WHERE user_id = ? AND quantity != 0 ORDER BY symbol",
                (user_id,),
            )
        return AccountSnapshot(
            user_id=user_id,
            cash=Decimal(cash["cash"]),
            positions=[Position(**r) for r in rows],
        )

    def _status(self, row: dict) -> BrokerOrderStatus:
        fills = self._db.all(
            "SELECT * FROM broker_fills WHERE broker_order_id = ? ORDER BY fill_id",
            (row["broker_order_id"],),
        )
        return BrokerOrderStatus(
            **{k: row[k] for k in BrokerOrderStatus.model_fields if k in row},
            fills=[
                Fill(
                    fill_id=f["fill_id"],
                    client_order_id=row["client_order_id"],
                    quantity=f["quantity"],
                    price=f["price"],
                    venue_time=f["venue_time"],
                )
                for f in fills
            ],
        )

    def get_open_orders(self, user_id: str) -> list[BrokerOrderStatus]:
        self._check_connection()
        rows = self._db.all(
            "SELECT * FROM broker_orders WHERE user_id = ? AND state IN (?, ?) ORDER BY created_at",
            (user_id, *OPEN_STATES),
        )
        return [self._status(r) for r in rows]

    def get_order(self, client_order_id: str) -> BrokerOrderStatus | None:
        """Look up by idempotency key; also gives resting orders a chance to match."""
        self._check_connection()
        with self._db.transaction():
            row = self._row(client_order_id)
            if row is None:
                return None
            if row["state"] in OPEN_STATES:
                self._match(row)
                row = self._row(client_order_id)
            return self._status(row)

    def _row(self, client_order_id: str) -> dict | None:
        return self._db.one(
            "SELECT * FROM broker_orders WHERE client_order_id = ?", (client_order_id,)
        )

    # -- writes -------------------------------------------------------------
    def submit_order(self, request: BrokerOrderRequest) -> BrokerOrderStatus:
        self._check_connection()
        if self.faults.fail_next_submits > 0:
            self.faults.fail_next_submits -= 1
            raise BrokerUnavailable("order entry transient failure")
        if self.faults.order_entry_down:
            raise BrokerUnavailable("order entry gateway down")
        with self._db.transaction():
            existing = self._row(request.client_order_id)
            if existing is None:
                self._ensure_account(request.user_id)
                reject = self._pre_accept_reject_reason(request)
                self._db.insert(
                    "broker_orders",
                    {
                        "broker_order_id": self._new_id("brk"),
                        "client_order_id": request.client_order_id,
                        "user_id": request.user_id,
                        "symbol": request.symbol,
                        "side": request.side.value,
                        "order_type": request.order_type.value,
                        "quantity": request.quantity,
                        "limit_price": None
                        if request.limit_price is None
                        else str(request.limit_price),
                        "state": "rejected" if reject else "accepted",
                        "filled_qty": 0,
                        "reject_reason": reject,
                        "created_at": self._clock.now().isoformat(),
                    },
                )
                row = self._row(request.client_order_id)
                if not reject:
                    self._match(row)
            status = self._status(self._row(request.client_order_id))
        if self.faults.drop_next_ack:
            # The order is on the books, but the caller never hears about it.
            self.faults.drop_next_ack = False
            raise BrokerUnavailable("connection lost before acknowledgement")
        return status

    def cancel_order(self, client_order_id: str) -> BrokerOrderStatus | None:
        self._check_connection()
        with self._db.transaction():
            row = self._row(client_order_id)
            if row is None:
                return None
            if row["state"] in OPEN_STATES:
                self._db.update(
                    "broker_orders", "client_order_id", client_order_id, {"state": "canceled"}
                )
            return self._status(self._row(client_order_id))

    # -- internals ----------------------------------------------------------
    def _pre_accept_reject_reason(self, req: BrokerOrderRequest) -> str | None:
        if req.quantity <= 0:
            return "invalid quantity"
        account = self.get_account(req.user_id)
        if req.side is Side.SELL:
            if account.position_qty(req.symbol) < req.quantity:
                return "insufficient position (short selling is not supported)"
            return None
        price = req.limit_price
        if price is None:
            try:
                price = self._md.get_quote(req.symbol).ask
            except (MarketDataUnavailable, KeyError):
                return "no market for symbol"
        if price * req.quantity > account.cash:
            return "insufficient buying power"
        return None

    def _match(self, row: dict) -> None:
        """Fill against the current simulated quote when the order is marketable."""
        try:
            quote = self._md.get_quote(row["symbol"])
        except (MarketDataUnavailable, KeyError):
            return
        side = Side(row["side"])
        limit = Decimal(row["limit_price"]) if row["limit_price"] is not None else None
        price = quote.ask if side is Side.BUY else quote.bid
        if OrderType(row["order_type"]) is OrderType.LIMIT:
            marketable = limit >= price if side is Side.BUY else limit <= price
            if not marketable:
                return
        remaining = row["quantity"] - row["filled_qty"]
        qty = remaining if self._max_fill is None else min(remaining, self._max_fill)
        if qty <= 0:
            return

        user_id, symbol = row["user_id"], row["symbol"]
        cash = Decimal(
            self._db.one("SELECT cash FROM broker_accounts WHERE user_id = ?", (user_id,))["cash"]
        )
        pos = self._db.one(
            "SELECT quantity, avg_cost FROM broker_positions WHERE user_id = ? AND symbol = ?",
            (user_id, symbol),
        ) or {"quantity": 0, "avg_cost": "0"}
        held, avg_cost = pos["quantity"], Decimal(pos["avg_cost"])
        if side is Side.BUY:
            if price * qty > cash:
                return
            new_qty = held + qty
            new_cost = ((avg_cost * held + price * qty) / new_qty).quantize(
                CENT / 100, rounding=ROUND_HALF_UP
            )
            cash -= price * qty
        else:
            if qty > held:
                return
            new_qty = held - qty
            new_cost = avg_cost if new_qty else Decimal("0")
            cash += price * qty

        filled = row["filled_qty"] + qty
        n = self._db.one(
            "SELECT COUNT(*) AS n FROM broker_fills WHERE broker_order_id = ?",
            (row["broker_order_id"],),
        )["n"]
        self._db.insert(
            "broker_fills",
            {
                "fill_id": f"{row['broker_order_id']}-{n + 1:03d}",
                "broker_order_id": row["broker_order_id"],
                "quantity": qty,
                "price": str(price),
                "venue_time": quote.event_time.isoformat(),
            },
        )
        self._db.update(
            "broker_orders",
            "broker_order_id",
            row["broker_order_id"],
            {
                "filled_qty": filled,
                "state": "filled" if filled == row["quantity"] else "partially_filled",
            },
        )
        self._db.update("broker_accounts", "user_id", user_id, {"cash": str(cash)})
        self._db.execute(
            "INSERT INTO broker_positions (user_id, symbol, quantity, avg_cost) VALUES (?,?,?,?) "
            "ON CONFLICT(user_id, symbol) DO UPDATE SET quantity = excluded.quantity, "
            "avg_cost = excluded.avg_cost",
            (user_id, symbol, new_qty, str(new_cost)),
        )
