"""Deterministic pre-trade risk and policy gate.

`evaluate` is a pure function of its inputs: no I/O, no clock reads, no randomness.
Every check runs and reports, so a decision lists all violations rather than the first.
Nothing here looks at planner confidence or rationale.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, time
from decimal import Decimal
from zoneinfo import ZoneInfo

from .broker.base import BrokerOrderStatus
from .config import Settings
from .domain import (
    TICK,
    AccountSnapshot,
    CheckResult,
    OrderType,
    Quote,
    Side,
    TradeProposal,
)

NEW_YORK = ZoneInfo("America/New_York")
SESSION_OPEN = time(9, 30)
SESSION_CLOSE = time(16, 0)


@dataclass(frozen=True)
class PolicyInputs:
    proposal: TradeProposal
    now: datetime
    quote: Quote | None  # None when market data is unavailable
    account: AccountSnapshot | None  # None when the broker is unreachable
    open_orders: list[BrokerOrderStatus]
    recent_similar: list[TradeProposal]  # other live proposals with identical parameters


def is_regular_session(now: datetime) -> bool:
    """NYSE cash session, Mon-Fri 09:30-16:00 New York time. Holidays are not modelled."""
    local = now.astimezone(NEW_YORK)
    return local.weekday() < 5 and SESSION_OPEN <= local.time() < SESSION_CLOSE


def order_notional(proposal: TradeProposal, quote: Quote, collar_bps: Decimal) -> Decimal:
    """Worst-case cash value of the order."""
    if proposal.order_type is OrderType.LIMIT and proposal.limit_price is not None:
        return proposal.limit_price * proposal.quantity
    if proposal.side is Side.BUY:
        return quote.ask * (1 + collar_bps / Decimal(10000)) * proposal.quantity
    return quote.bid * proposal.quantity


class PolicyService:
    def __init__(self, settings: Settings) -> None:
        self._s = settings

    @property
    def version(self) -> str:
        return self._s.policy_version

    def evaluate(self, inp: PolicyInputs) -> list[CheckResult]:
        s, p = self._s, inp.proposal
        checks: list[CheckResult] = []

        def check(code: str, passed: bool, detail: str) -> None:
            checks.append(CheckResult(code=code, passed=passed, detail=detail))

        check("MODE_NOT_PAPER", p.mode == "paper", f"mode is {p.mode!r}; only 'paper' may trade")
        check(
            "SYMBOL_NOT_ALLOWED",
            p.symbol in s.universe,
            f"{p.symbol} {'is' if p.symbol in s.universe else 'is not'} in the tradable universe",
        )
        check("INVALID_QUANTITY", p.quantity > 0, f"quantity is {p.quantity}")

        price_ok = True
        detail = "market order has no limit price"
        if p.order_type is OrderType.LIMIT:
            price_ok = p.limit_price is not None and p.limit_price > 0 and p.limit_price % TICK == 0
            detail = f"limit price is {p.limit_price} (must be positive, on a {TICK} tick)"
        elif p.limit_price is not None:
            price_ok, detail = False, "market order must not carry a limit price"
        check("INVALID_PRICE", price_ok, detail)

        session_ok = s.market_session == "always" or is_regular_session(inp.now)
        check(
            "MARKET_CLOSED",
            session_ok,
            f"session policy {s.market_session!r} at {inp.now.astimezone(NEW_YORK):%a %H:%M} ET",
        )

        check(
            "DUPLICATE_ORDER",
            not inp.recent_similar,
            f"{len(inp.recent_similar)} identical live proposal(s) within "
            f"{s.duplicate_window_seconds}s"
            + (f": {', '.join(r.id for r in inp.recent_similar)}" if inp.recent_similar else ""),
        )

        quote = inp.quote
        if quote is None:
            check("QUOTE_UNAVAILABLE", False, "no quote available; cannot price the order")
        else:
            check("QUOTE_UNAVAILABLE", True, f"quote received at {quote.received_at.isoformat()}")
            age = Decimal(str((inp.now - quote.event_time).total_seconds()))
            check(
                "QUOTE_STALE",
                age <= s.max_quote_age_seconds,
                f"quote is {age}s old (limit {s.max_quote_age_seconds}s)",
            )
            check(
                "QUOTE_CLOCK_SKEW",
                age >= -s.max_clock_skew_seconds,
                f"venue time is {-age}s ahead of local (tolerance {s.max_clock_skew_seconds}s)",
            )
            if p.order_type is OrderType.LIMIT and p.limit_price is not None and price_ok:
                deviation = abs(p.limit_price - quote.mid) / quote.mid * 100
                check(
                    "LIMIT_PRICE_BAND",
                    deviation <= s.limit_price_band_pct,
                    f"limit {p.limit_price} is {deviation:.2f}% from mid {quote.mid} "
                    f"(band {s.limit_price_band_pct}%)",
                )

        account = inp.account
        if account is None:
            check("PORTFOLIO_UNAVAILABLE", False, "broker unreachable; cannot verify exposure")
        elif quote is not None and price_ok and p.quantity > 0:
            check("PORTFOLIO_UNAVAILABLE", True, "portfolio snapshot loaded")
            notional = order_notional(p, quote, s.market_order_collar_bps)
            check(
                "MAX_ORDER_NOTIONAL",
                notional <= s.max_order_notional,
                f"order notional {notional:.2f} (limit {s.max_order_notional})",
            )
            same_symbol = [o for o in inp.open_orders if o.symbol == p.symbol]
            open_buy_qty = sum(o.quantity - o.filled_qty for o in same_symbol if o.side is Side.BUY)
            open_sell_qty = sum(
                o.quantity - o.filled_qty for o in same_symbol if o.side is Side.SELL
            )
            held = account.position_qty(p.symbol)
            if p.side is Side.BUY:
                post_value = (held + open_buy_qty) * quote.mid + notional
                check(
                    "MAX_POSITION_NOTIONAL",
                    post_value <= s.max_position_notional,
                    f"post-trade {p.symbol} exposure {post_value:.2f} "
                    f"(limit {s.max_position_notional})",
                )
                reserved = sum(
                    (o.limit_price or quote.ask) * (o.quantity - o.filled_qty)
                    for o in inp.open_orders
                    if o.side is Side.BUY
                )
                available = account.cash - reserved
                check(
                    "INSUFFICIENT_BUYING_POWER",
                    notional <= available,
                    f"needs {notional:.2f}, available {available:.2f}",
                )
            else:
                sellable = held - open_sell_qty
                check(
                    "INSUFFICIENT_POSITION",
                    p.quantity <= sellable,
                    f"selling {p.quantity}, sellable {sellable} (short selling not allowed)",
                )
        elif account is not None:
            check("PORTFOLIO_UNAVAILABLE", True, "portfolio snapshot loaded")

        return checks
