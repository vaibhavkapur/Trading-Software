import pytest

from atp.domain import OrderType, Side
from atp.planner import RuleBasedPlanner

SYMBOLS = ["SPY", "AAPL"]


@pytest.mark.parametrize(
    "text, expected",
    [
        (
            "Buy $500 of SPY using a limit order",
            dict(
                side=Side.BUY,
                symbol="SPY",
                notional="500",
                quantity=None,
                order_type=OrderType.LIMIT,
                limit_price=None,
            ),
        ),
        (
            "sell 10 shares of aapl at 47.80",
            dict(
                side=Side.SELL,
                symbol="AAPL",
                notional=None,
                quantity=10,
                order_type=OrderType.LIMIT,
                limit_price="47.80",
            ),
        ),
        (
            "Buy 5 SPY at market",
            dict(
                side=Side.BUY,
                symbol="SPY",
                notional=None,
                quantity=5,
                order_type=OrderType.MARKET,
                limit_price=None,
            ),
        ),
        (
            "please purchase $1,250.50 worth of SPY, limit 124.10",
            dict(
                side=Side.BUY,
                symbol="SPY",
                notional="1250.50",
                quantity=None,
                order_type=OrderType.LIMIT,
                limit_price="124.10",
            ),
        ),
    ],
)
def test_parses_supported_phrasings(text, expected):
    parsed = RuleBasedPlanner().plan(text, SYMBOLS)
    assert parsed.actionable
    for field, value in expected.items():
        assert getattr(parsed, field) == value, field


@pytest.mark.parametrize("text", ["What do you think of SPY?", "buy some stuff", "sell everything"])
def test_unclear_text_is_not_actionable(text):
    parsed = RuleBasedPlanner().plan(text, SYMBOLS)
    assert not parsed.actionable
    assert parsed.clarification
