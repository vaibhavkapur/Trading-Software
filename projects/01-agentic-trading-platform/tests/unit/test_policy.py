"""The policy gate is a pure function, so every boundary is tested without I/O."""

from datetime import UTC, datetime, timedelta
from decimal import Decimal

import pytest

from atp.broker.base import BrokerOrderStatus
from atp.config import Settings
from atp.domain import (
    AccountSnapshot,
    OrderType,
    Position,
    ProposalStatus,
    Quote,
    Side,
    TradeProposal,
)
from atp.policy import PolicyInputs, PolicyService, is_regular_session

NOW = datetime(2026, 1, 5, 15, 0, 0, tzinfo=UTC)  # Mon 10:00 ET
POLICY = PolicyService(Settings(approval_secret=b"x"))


def proposal(**kw) -> TradeProposal:
    base = dict(
        id="prp_1",
        intent_id="int_1",
        correlation_id="cor_1",
        user_id="alice",
        mode="paper",
        symbol="SPY",
        side=Side.BUY,
        order_type=OrderType.LIMIT,
        quantity=10,
        limit_price=Decimal("100.00"),
        reference_price=Decimal("100.00"),
        quote_asof=NOW,
        quote_received_at=NOW,
        status=ProposalStatus.DRAFT,
        proposal_hash="sha256:x",
        created_at=NOW,
        updated_at=NOW,
    )
    return TradeProposal(**{**base, **kw})


def quote(age: float = 0, mid: str = "100.00") -> Quote:
    m = Decimal(mid)
    return Quote(
        symbol="SPY",
        bid=m - Decimal("0.01"),
        ask=m + Decimal("0.01"),
        last=m,
        event_time=NOW - timedelta(seconds=age),
        received_at=NOW,
    )


def account(cash="100000", **positions) -> AccountSnapshot:
    return AccountSnapshot(
        user_id="alice",
        cash=Decimal(cash),
        positions=[
            Position(symbol=s, quantity=q, avg_cost=Decimal("100")) for s, q in positions.items()
        ],
    )


def violations(p=None, *, now=NOW, q="default", acct="default", open_orders=(), similar=()):
    checks = POLICY.evaluate(
        PolicyInputs(
            proposal=p or proposal(),
            now=now,
            quote=quote() if q == "default" else q,
            account=account() if acct == "default" else acct,
            open_orders=list(open_orders),
            recent_similar=list(similar),
        )
    )
    return {c.code for c in checks if not c.passed}


def test_clean_proposal_passes_every_check():
    assert violations() == set()


def test_quote_age_boundary():
    assert violations(q=quote(age=5)) == set()  # exactly at the limit
    assert violations(q=quote(age=5.001)) == {"QUOTE_STALE"}


def test_clock_skew_boundary():
    assert violations(q=quote(age=-2)) == set()
    assert violations(q=quote(age=-2.5)) == {"QUOTE_CLOCK_SKEW"}


def test_missing_quote_fails_closed():
    assert violations(q=None) == {"QUOTE_UNAVAILABLE"}


def test_unreachable_broker_fails_closed():
    assert violations(acct=None) == {"PORTFOLIO_UNAVAILABLE"}


def test_order_notional_boundary():
    assert violations(proposal(quantity=100)) == set()  # exactly 10,000
    assert "MAX_ORDER_NOTIONAL" in violations(proposal(quantity=101))


def test_position_limit_counts_holdings_and_working_orders():
    held = account(SPY=200)  # ~20,000 already held
    assert violations(proposal(quantity=49), acct=held) == set()
    assert violations(proposal(quantity=51), acct=held) == {"MAX_POSITION_NOTIONAL"}
    working = BrokerOrderStatus(
        broker_order_id="b1",
        client_order_id="o1",
        user_id="alice",
        symbol="SPY",
        side=Side.BUY,
        order_type=OrderType.LIMIT,
        quantity=10,
        limit_price=Decimal("100"),
        state="accepted",
        filled_qty=0,
    )
    assert violations(proposal(quantity=45), acct=held, open_orders=[working]) == {
        "MAX_POSITION_NOTIONAL"
    }


def test_buying_power_boundary():
    assert violations(acct=account(cash="1000.00")) == set()
    assert violations(acct=account(cash="999.99")) == {"INSUFFICIENT_BUYING_POWER"}


def test_no_short_selling():
    sell = proposal(side=Side.SELL)
    assert violations(sell, acct=account(SPY=10)) == set()
    assert violations(sell, acct=account(SPY=9)) == {"INSUFFICIENT_POSITION"}
    assert violations(sell) == {"INSUFFICIENT_POSITION"}


def test_limit_price_band():
    assert violations(proposal(limit_price=Decimal("105.00"))) == set()
    assert violations(proposal(limit_price=Decimal("105.01"))) == {"LIMIT_PRICE_BAND"}
    assert violations(proposal(limit_price=Decimal("94.99"))) == {"LIMIT_PRICE_BAND"}


@pytest.mark.parametrize(
    "kw, code",
    [
        (dict(mode="live"), "MODE_NOT_PAPER"),
        (dict(symbol="GME"), "SYMBOL_NOT_ALLOWED"),
        (dict(quantity=0), "INVALID_QUANTITY"),
        (dict(quantity=-5), "INVALID_QUANTITY"),
        (dict(limit_price=Decimal("0")), "INVALID_PRICE"),
        (dict(limit_price=Decimal("100.001")), "INVALID_PRICE"),
        (dict(limit_price=None), "INVALID_PRICE"),
        (dict(order_type=OrderType.MARKET), "INVALID_PRICE"),  # market with a limit price
    ],
)
def test_malformed_proposals_are_rejected(kw, code):
    assert code in violations(proposal(**kw))


def test_market_order_uses_collared_ask():
    p = proposal(order_type=OrderType.MARKET, limit_price=None, quantity=99)
    assert violations(p) == set()  # 99 * 100.01 * 1.005 = 9950.49
    assert violations(proposal(order_type=OrderType.MARKET, limit_price=None, quantity=100)) == {
        "MAX_ORDER_NOTIONAL"
    }


def test_duplicate_is_flagged():
    assert violations(similar=[proposal(id="prp_0")]) == {"DUPLICATE_ORDER"}


def test_all_violations_are_reported_together():
    found = violations(proposal(mode="live", quantity=1000), q=quote(age=60))
    assert {"MODE_NOT_PAPER", "QUOTE_STALE", "MAX_ORDER_NOTIONAL"} <= found


@pytest.mark.parametrize(
    "when, open_",
    [
        (datetime(2026, 1, 5, 14, 29, 59, tzinfo=UTC), False),  # 09:29:59 ET
        (datetime(2026, 1, 5, 14, 30, 0, tzinfo=UTC), True),  # 09:30:00 ET
        (datetime(2026, 1, 5, 20, 59, 59, tzinfo=UTC), True),  # 15:59:59 ET
        (datetime(2026, 1, 5, 21, 0, 0, tzinfo=UTC), False),  # 16:00:00 ET
        (datetime(2026, 1, 3, 15, 0, 0, tzinfo=UTC), False),  # Saturday
        (datetime(2026, 7, 6, 13, 30, 0, tzinfo=UTC), True),  # 09:30 EDT (DST)
    ],
)
def test_trading_hours(when, open_):
    assert is_regular_session(when) is open_
    assert (
        "MARKET_CLOSED"
        in violations(
            now=when, q=quote(age=0).model_copy(update={"event_time": when, "received_at": when})
        )
    ) is (not open_)
