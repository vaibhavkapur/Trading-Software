from decimal import Decimal

import pytest

from atp.domain import (
    ORDER_TRANSITIONS,
    TERMINAL_ORDER_STATES,
    IllegalTransition,
    OrderState,
    check_transition,
    compute_proposal_hash,
)

BASE = dict(
    proposal_id="prp_1",
    intent_id="int_1",
    user_id="alice",
    mode="paper",
    symbol="SPY",
    side="buy",
    order_type="limit",
    quantity=3,
    limit_price=Decimal("125.25"),
    time_in_force="day",
)


def test_hash_is_stable_and_ignores_decimal_representation():
    assert compute_proposal_hash(**BASE) == compute_proposal_hash(
        **{**BASE, "limit_price": Decimal("125.250")}
    )


@pytest.mark.parametrize(
    "field, value",
    [
        ("quantity", 4),
        ("limit_price", Decimal("125.26")),
        ("limit_price", None),
        ("symbol", "QQQ"),
        ("side", "sell"),
        ("order_type", "market"),
        ("user_id", "mallory"),
        ("mode", "live"),
        ("proposal_id", "prp_2"),
        ("time_in_force", "gtc"),
    ],
)
def test_any_parameter_change_changes_the_hash(field, value):
    assert compute_proposal_hash(**BASE) != compute_proposal_hash(**{**BASE, field: value})


def test_terminal_states_have_no_exits():
    for state in TERMINAL_ORDER_STATES:
        assert ORDER_TRANSITIONS[state] == frozenset()


@pytest.mark.parametrize(
    "current, new",
    [
        (OrderState.FILLED, OrderState.ACCEPTED),
        (OrderState.CANCELED, OrderState.FILLED),
        (OrderState.REJECTED, OrderState.SUBMITTING),
        (OrderState.CREATED, OrderState.FILLED),  # cannot fill without dispatch
        (OrderState.CREATED, OrderState.ACCEPTED),
        (OrderState.PARTIALLY_FILLED, OrderState.REJECTED),
    ],
)
def test_illegal_transitions_raise(current, new):
    with pytest.raises(IllegalTransition):
        check_transition(current, new)


def test_legal_path():
    for current, new in [
        (OrderState.CREATED, OrderState.SUBMITTING),
        (OrderState.SUBMITTING, OrderState.ACCEPTED),
        (OrderState.ACCEPTED, OrderState.PARTIALLY_FILLED),
        (OrderState.PARTIALLY_FILLED, OrderState.PARTIALLY_FILLED),
        (OrderState.PARTIALLY_FILLED, OrderState.FILLED),
    ]:
        check_transition(current, new)
