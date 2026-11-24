"""Scenario tests for the README's definition of done.

1. An unapproved or changed order can never execute.
2. Every executed order links to a user intent, policy decision and approval.
3. Duplicate submits resolve to one logical broker order.
4. A stale quote, expired approval or disconnected broker produces a safe rejection.
"""

import sqlite3
from concurrent.futures import ThreadPoolExecutor

import pytest
from conftest import USER, approved_order

from atp.domain import OrderState, ProposalStatus
from atp.oms import BrokerUnavailableError
from atp.service import Conflict, NotFound


def broker_orders(svc) -> list[dict]:
    return svc.db.all("SELECT * FROM broker_orders")


def assert_nothing_executed(svc):
    assert broker_orders(svc) == []
    assert svc.db.all("SELECT * FROM broker_fills") == []
    assert svc.broker.get_account(USER).cash == svc.settings.starting_cash


# -- 1. unapproved or changed orders ----------------------------------------
def test_no_order_exists_before_approval(svc):
    _, proposal = svc.create_intent(USER, "Buy $500 of SPY")
    svc.evaluate(USER, proposal.id)
    assert svc.store.order_for_proposal(proposal.id) is None
    with pytest.raises(NotFound):
        svc.submit_order(USER, "ord_guess")
    assert_nothing_executed(svc)


def test_cannot_approve_without_passing_risk(svc):
    _, proposal = svc.create_intent(USER, "Buy $500 of SPY")
    with pytest.raises(Conflict) as exc:
        svc.decide(USER, proposal.id, proposal.proposal_hash, "approve")
    assert exc.value.code == "RISK_NOT_PASSED"

    _, big = svc.create_intent(USER, "Buy 5000 shares of SPY")
    assert not svc.evaluate(USER, big.id).passed
    with pytest.raises(Conflict) as exc:
        svc.decide(USER, big.id, big.proposal_hash, "approve")
    assert exc.value.code == "RISK_NOT_PASSED"
    assert_nothing_executed(svc)


def test_approval_must_present_the_reviewed_hash(svc):
    _, proposal = svc.create_intent(USER, "Buy $500 of SPY")
    svc.evaluate(USER, proposal.id)
    with pytest.raises(Conflict) as exc:
        svc.decide(USER, proposal.id, "sha256:" + "0" * 64, "approve")
    assert exc.value.code == "PROPOSAL_HASH_MISMATCH"
    assert svc.store.approval_for(proposal.id) is None


def test_rejected_proposal_cannot_execute(svc):
    _, proposal = svc.create_intent(USER, "Buy $500 of SPY")
    svc.evaluate(USER, proposal.id)
    _, order = svc.decide(USER, proposal.id, proposal.proposal_hash, "reject")
    assert order is None
    with pytest.raises(Conflict):
        svc.decide(USER, proposal.id, proposal.proposal_hash, "approve")
    assert_nothing_executed(svc)


def test_order_row_cannot_be_forged_without_an_approval(svc):
    _, proposal = svc.create_intent(USER, "Buy $500 of SPY")
    with pytest.raises(sqlite3.IntegrityError):
        svc.db.execute(
            "INSERT INTO orders (client_order_id, proposal_id, approval_id, correlation_id, "
            "user_id, symbol, side, order_type, quantity, limit_price, state, created_at, "
            "updated_at) VALUES ('ord_x', ?, 'apr_forged', 'c', ?, 'SPY', 'buy', 'limit', 3, "
            "'125.25', 'created', '2026-01-05T15:00:00Z', '2026-01-05T15:00:00Z')",
            (proposal.id, USER),
        )


def test_forged_approval_row_is_rejected_at_submit(svc):
    """An attacker with database write access still cannot mint an approval."""
    _, proposal = svc.create_intent(USER, "Buy $500 of SPY")
    svc.db.execute(
        "INSERT INTO approvals VALUES ('apr_forged', ?, ?, ?, 'approve', "
        "'2026-01-05T15:00:00Z', '2099-01-01T00:00:00Z', 'deadbeef')",
        (proposal.id, proposal.proposal_hash, USER),
    )
    svc.db.execute("UPDATE proposals SET status = 'approved' WHERE id = ?", (proposal.id,))
    svc.db.execute(
        "INSERT INTO orders (client_order_id, proposal_id, approval_id, correlation_id, "
        "user_id, symbol, side, order_type, quantity, limit_price, state, created_at, "
        "updated_at) VALUES ('ord_x', ?, 'apr_forged', 'c', ?, 'SPY', 'buy', 'limit', 3, "
        "'125.25', 'created', '2026-01-05T15:00:00Z', '2026-01-05T15:00:00Z')",
        (proposal.id, USER),
    )
    order = svc.submit_order(USER, "ord_x")
    assert order.state is OrderState.REJECTED
    assert order.reject_reason == "APPROVAL_SIGNATURE_INVALID"
    assert_nothing_executed(svc)


@pytest.mark.parametrize(
    "tamper_sql, reason",
    [
        ("UPDATE proposals SET quantity = 30", "PROPOSAL_CHANGED"),
        ("UPDATE proposals SET limit_price = '130.00'", "PROPOSAL_CHANGED"),
        ("UPDATE proposals SET symbol = 'QQQ'", "PROPOSAL_CHANGED"),
        ("UPDATE proposals SET side = 'sell'", "PROPOSAL_CHANGED"),
        ("UPDATE proposals SET order_type = 'market', limit_price = NULL", "PROPOSAL_CHANGED"),
        ("UPDATE orders SET quantity = 30", "ORDER_CHANGED"),
        ("UPDATE orders SET symbol = 'QQQ'", "ORDER_CHANGED"),
        ("UPDATE approvals SET expires_at = '2099-01-01T00:00:00Z'", "APPROVAL_SIGNATURE_INVALID"),
        ("UPDATE proposals SET status = 'risk_passed'", "PROPOSAL_NOT_APPROVED"),
    ],
)
def test_order_changed_after_approval_never_executes(svc, tamper_sql, reason):
    _, order = approved_order(svc)
    svc.db.execute(tamper_sql)
    result = svc.submit_order(USER, order.client_order_id)
    assert result.state is OrderState.REJECTED
    assert result.reject_reason == reason
    assert_nothing_executed(svc)
    # Rejection is terminal: retrying cannot revive it.
    assert svc.submit_order(USER, order.client_order_id).state is OrderState.REJECTED
    assert_nothing_executed(svc)


def test_changing_parameters_and_both_hashes_still_fails(svc):
    proposal, order = approved_order(svc)
    svc.db.execute("UPDATE proposals SET quantity = 30, proposal_hash = 'sha256:new'")
    svc.db.execute("UPDATE approvals SET proposal_hash = 'sha256:new'")
    svc.db.execute("UPDATE orders SET quantity = 30")
    result = svc.submit_order(USER, order.client_order_id)
    assert result.reject_reason == "APPROVAL_SIGNATURE_INVALID"
    assert_nothing_executed(svc)


def test_other_users_cannot_see_or_act(svc):
    proposal, order = approved_order(svc)
    for call in (
        lambda: svc.get_proposal("bob", proposal.id),
        lambda: svc.evaluate("bob", proposal.id),
        lambda: svc.decide("bob", proposal.id, proposal.proposal_hash, "approve"),
        lambda: svc.submit_order("bob", order.client_order_id),
        lambda: svc.get_order("bob", order.client_order_id),
        lambda: svc.cancel_order("bob", order.client_order_id),
    ):
        with pytest.raises(NotFound):
            call()
    assert svc.audit_trail("bob", proposal.correlation_id) == []
    assert_nothing_executed(svc)


# -- 2. traceability --------------------------------------------------------
def test_executed_order_links_intent_decision_and_approval(svc):
    proposal, order = approved_order(svc)
    done = svc.submit_order(USER, order.client_order_id)
    assert done.state is OrderState.FILLED

    row = svc.db.one(
        "SELECT o.client_order_id, i.id AS intent_id, i.text, a.id AS approval_id, "
        "a.approver_id, r.id AS risk_id, r.passed, r.policy_version "
        "FROM orders o JOIN proposals p ON p.id = o.proposal_id "
        "JOIN intents i ON i.id = p.intent_id JOIN approvals a ON a.id = o.approval_id "
        "JOIN risk_decisions r ON r.proposal_id = p.id AND r.stage = 'pre_submit' "
        "WHERE o.client_order_id = ?",
        (order.client_order_id,),
    )
    assert row["text"] == "Buy $500 of SPY using a limit order"
    assert row["approver_id"] == USER and row["passed"] == 1
    assert row["policy_version"] == svc.settings.policy_version

    events = [e["event_type"] for e in svc.audit_trail(USER, proposal.correlation_id)]
    for expected in (
        "intent.created",
        "proposal.created",
        "risk.evaluated",
        "approval.approved",
        "order.created",
        "order.dispatched",
        "order.fill",
    ):
        assert expected in events
    assert events.index("approval.approved") < events.index("order.dispatched")
    assert svc.store.proposal(proposal.id).status is ProposalStatus.EXECUTED
    assert svc.audit.verify_chain() == (True, None)


# -- 3. duplicate submits ---------------------------------------------------
def test_repeat_submit_is_one_broker_order(svc):
    _, order = approved_order(svc)
    first = svc.submit_order(USER, order.client_order_id)
    for _ in range(5):
        again = svc.submit_order(USER, order.client_order_id)
        assert again == first
    assert len(broker_orders(svc)) == 1
    assert len(svc.store.fills(order.client_order_id)) == 1
    assert svc.broker.get_account(USER).position_qty("SPY") == 3
    assert svc.metrics.value("atp_order_submissions_total", {"outcome": "duplicate"}) == 5


def test_concurrent_submits_are_one_broker_order(svc):
    _, order = approved_order(svc)
    with ThreadPoolExecutor(max_workers=8) as pool:
        results = list(pool.map(lambda _: svc.submit_order(USER, order.client_order_id), range(24)))
    assert {r.state for r in results} == {OrderState.FILLED}
    assert len(broker_orders(svc)) == 1
    assert svc.broker.get_account(USER).position_qty("SPY") == 3


def test_replayed_approval_creates_one_order(svc):
    _, proposal = svc.create_intent(USER, "Buy $500 of SPY")
    svc.evaluate(USER, proposal.id)
    a1, o1 = svc.decide(USER, proposal.id, proposal.proposal_hash, "approve")
    a2, o2 = svc.decide(USER, proposal.id, proposal.proposal_hash, "approve")
    assert (a1.id, o1.client_order_id) == (a2.id, o2.client_order_id)
    assert len(svc.db.all("SELECT * FROM orders")) == 1


def test_identical_second_trade_is_flagged_as_duplicate(svc):
    _, order = approved_order(svc)
    svc.submit_order(USER, order.client_order_id)
    _, again = svc.create_intent(USER, "Buy $500 of SPY using a limit order")
    decision = svc.evaluate(USER, again.id)
    assert [v.code for v in decision.violations] == ["DUPLICATE_ORDER"]


def test_lost_ack_resolves_to_one_broker_order(svc):
    """Broker books the order, the reply is lost, and order entry then stays down."""
    _, order = approved_order(svc)
    svc.broker.faults.drop_next_ack = True
    svc.broker.faults.order_entry_down = False
    svc.oms.max_submit_attempts = 1
    with pytest.raises(BrokerUnavailableError):
        svc.submit_order(USER, order.client_order_id)
    assert svc.store.order(order.client_order_id).state is OrderState.SUBMITTING
    assert len(broker_orders(svc)) == 1  # it did reach the broker

    recovered = svc.submit_order(USER, order.client_order_id)
    assert recovered.state is OrderState.FILLED
    assert len(broker_orders(svc)) == 1
    assert svc.broker.get_account(USER).position_qty("SPY") == 3
    assert (
        svc.metrics.value("atp_order_reconciliation_mismatch_total", {"kind": "unacked_order"}) == 1
    )


def test_lost_ack_is_recovered_by_in_call_retry(svc):
    _, order = approved_order(svc)
    svc.broker.faults.drop_next_ack = True
    done = svc.submit_order(USER, order.client_order_id)
    assert done.state is OrderState.FILLED and done.submit_attempts == 2
    assert len(broker_orders(svc)) == 1
    assert svc.metrics.value("atp_order_submit_retries_total") == 1


def test_lost_ack_then_expired_approval_still_reconciles(svc, clock):
    """The order is live at the broker; local expiry must not orphan it."""
    _, order = approved_order(svc)
    svc.broker.faults.drop_next_ack = True
    svc.oms.max_submit_attempts = 1
    with pytest.raises(BrokerUnavailableError):
        svc.submit_order(USER, order.client_order_id)
    clock.advance(svc.settings.approval_ttl_seconds + 1)
    assert svc.get_order(USER, order.client_order_id)["order"].state is OrderState.FILLED


def test_duplicate_fill_delivery_is_ingested_once(svc):
    _, order = approved_order(svc)
    done = svc.submit_order(USER, order.client_order_id)
    status = svc.broker.get_order(order.client_order_id)
    for _ in range(3):
        assert svc.oms._apply(done, status) == done
    assert len(svc.store.fills(order.client_order_id)) == 1


# -- 4. safe rejections -----------------------------------------------------
def test_stale_quote_at_submit_is_rejected(svc, clock):
    _, order = approved_order(svc)
    svc.market_data.stall()
    clock.advance(30)
    result = svc.submit_order(USER, order.client_order_id)
    assert result.state is OrderState.REJECTED
    assert result.reject_reason == "RISK:QUOTE_STALE"
    assert_nothing_executed(svc)
    assert svc.store.proposal(order.proposal_id).status is ProposalStatus.FAILED
    assert (
        svc.metrics.value(
            "atp_policy_rejections_total", {"reason": "QUOTE_STALE", "stage": "pre_submit"}
        )
        == 1
    )


def test_stale_quote_at_evaluation_is_rejected(svc, clock):
    _, proposal = svc.create_intent(USER, "Buy $500 of SPY")
    svc.market_data.stall()
    clock.advance(6)
    decision = svc.evaluate(USER, proposal.id)
    assert [v.code for v in decision.violations] == ["QUOTE_STALE"]
    assert svc.store.proposal(proposal.id).status is ProposalStatus.RISK_REJECTED
    # Feed recovers: the same proposal can be re-evaluated with fresh data.
    svc.market_data.resume()
    assert svc.evaluate(USER, proposal.id).passed


def test_missing_market_data_is_rejected(svc):
    _, order = approved_order(svc)
    svc.market_data.faults.down = True
    assert svc.submit_order(USER, order.client_order_id).reject_reason == "RISK:QUOTE_UNAVAILABLE"
    assert_nothing_executed(svc)
    with pytest.raises(Exception) as exc:
        svc.create_intent(USER, "Buy $500 of QQQ")
    assert exc.value.code == "MARKET_DATA_UNAVAILABLE"


def test_clock_skew_is_rejected(svc):
    _, order = approved_order(svc)
    svc.market_data.faults.skew_seconds = 30  # venue timestamps from the future
    assert svc.submit_order(USER, order.client_order_id).reject_reason == "RISK:QUOTE_CLOCK_SKEW"
    assert_nothing_executed(svc)


def test_price_moved_away_since_approval(svc):
    _, order = approved_order(svc)
    svc.market_data.faults.price_shock_pct = -10
    result = svc.submit_order(USER, order.client_order_id)
    assert result.reject_reason == "RISK:LIMIT_PRICE_BAND"
    assert_nothing_executed(svc)


def test_expired_approval_is_rejected(svc, clock):
    _, order = approved_order(svc)
    clock.advance(svc.settings.approval_ttl_seconds)
    result = svc.submit_order(USER, order.client_order_id)
    assert result.state is OrderState.REJECTED
    assert result.reject_reason == "APPROVAL_EXPIRED"
    assert_nothing_executed(svc)


def test_disconnected_broker_is_rejected_before_dispatch(svc):
    _, order = approved_order(svc)
    svc.broker.faults.disconnected = True
    result = svc.submit_order(USER, order.client_order_id)
    assert result.state is OrderState.REJECTED
    assert result.reject_reason == "RISK:PORTFOLIO_UNAVAILABLE"
    svc.broker.faults.disconnected = False
    assert_nothing_executed(svc)


def test_order_entry_outage_is_retryable_and_never_assumes_a_fill(svc):
    _, order = approved_order(svc)
    svc.broker.faults.order_entry_down = True
    with pytest.raises(BrokerUnavailableError):
        svc.submit_order(USER, order.client_order_id)
    pending = svc.store.order(order.client_order_id)
    assert pending.state is OrderState.SUBMITTING and pending.filled_qty == 0
    assert pending.submit_attempts == svc.settings.max_submit_attempts
    assert_nothing_executed(svc)
    assert svc.metrics.value("atp_order_submit_retries_total") == 2

    svc.broker.faults.order_entry_down = False
    done = svc.submit_order(USER, order.client_order_id)
    assert done.state is OrderState.FILLED
    assert len(broker_orders(svc)) == 1


def test_outage_outlasting_the_approval_ends_in_rejection(svc, clock):
    _, order = approved_order(svc)
    svc.broker.faults.order_entry_down = True
    with pytest.raises(BrokerUnavailableError):
        svc.submit_order(USER, order.client_order_id)
    clock.advance(svc.settings.approval_ttl_seconds + 1)
    svc.broker.faults.order_entry_down = False
    result = svc.submit_order(USER, order.client_order_id)
    assert result.reject_reason == "APPROVAL_EXPIRED"
    assert_nothing_executed(svc)


def test_transient_dispatch_failure_is_retried(svc):
    _, order = approved_order(svc)
    svc.broker.faults.fail_next_submits = 2
    done = svc.submit_order(USER, order.client_order_id)
    assert done.state is OrderState.FILLED and done.submit_attempts == 3
    assert len(broker_orders(svc)) == 1


def test_market_closed_is_rejected(svc, clock):
    clock.advance(8 * 3600)  # 18:00 ET
    _, proposal = svc.create_intent(USER, "Buy $500 of SPY")
    assert [v.code for v in svc.evaluate(USER, proposal.id).violations] == ["MARKET_CLOSED"]


def test_broker_side_rejection_is_recorded(svc):
    """Policy passes on a snapshot; the broker is still the final authority."""
    _, order = approved_order(svc)
    svc.db.execute("UPDATE broker_accounts SET cash = '10.00'")
    real = svc.broker_read._get_account  # make the policy gate see a stale, richer snapshot
    svc.broker_read._get_account = lambda user: real(user).model_copy(
        update={"cash": svc.settings.starting_cash}
    )
    result = svc.submit_order(USER, order.client_order_id)
    assert result.state is OrderState.REJECTED
    assert result.reject_reason == "BROKER:insufficient buying power"
    assert svc.db.all("SELECT * FROM broker_fills") == []
