import sqlite3
from datetime import timedelta

import pytest
from conftest import USER, approved_order

from atp.approvals import ApprovalInvalid
from atp.domain import Decision
from atp.tools import WRITE, Principal, Tool, ToolNotAllowed


def test_valid_approval_verifies(svc, clock):
    proposal, order = approved_order(svc)
    approval = svc.store.approval(order.approval_id)
    svc.approvals.verify(approval, proposal, clock.now())


def _code(svc, approval, proposal, now) -> str:
    with pytest.raises(ApprovalInvalid) as exc:
        svc.approvals.verify(approval, proposal, now)
    return exc.value.code


def test_approval_expiry_boundary(svc, clock):
    proposal, order = approved_order(svc)
    approval = svc.store.approval(order.approval_id)
    just_before = approval.expires_at - timedelta(microseconds=1)
    svc.approvals.verify(approval, proposal, just_before)
    assert _code(svc, approval, proposal, approval.expires_at) == "APPROVAL_EXPIRED"


def test_approval_is_bound_to_parameters(svc, clock):
    proposal, order = approved_order(svc)
    approval = svc.store.approval(order.approval_id)
    changed = proposal.model_copy(update={"quantity": proposal.quantity + 1})
    assert _code(svc, approval, changed, clock.now()) == "PROPOSAL_CHANGED"


def test_forged_or_edited_approval_fails_signature(svc, clock):
    proposal, order = approved_order(svc)
    approval = svc.store.approval(order.approval_id)
    extended = approval.model_copy(update={"expires_at": approval.expires_at + timedelta(days=1)})
    assert _code(svc, extended, proposal, clock.now()) == "APPROVAL_SIGNATURE_INVALID"
    rehashed = approval.model_copy(update={"proposal_hash": "sha256:" + "0" * 64})
    assert _code(svc, rehashed, proposal, clock.now()) == "APPROVAL_SIGNATURE_INVALID"


def test_rejection_does_not_authorise(svc, clock):
    _, proposal = svc.create_intent(USER, "Buy $500 of SPY")
    approval, order = svc.decide(USER, proposal.id, proposal.proposal_hash, Decision.REJECT)
    assert order is None
    assert _code(svc, approval, proposal, clock.now()) == "NOT_APPROVED"


def test_audit_chain_verifies_and_table_is_append_only(svc):
    approved_order(svc)
    assert svc.audit.verify_chain() == (True, None)
    with pytest.raises(sqlite3.IntegrityError):
        svc.db.execute("UPDATE audit_events SET actor = 'mallory' WHERE seq = 1")
    with pytest.raises(sqlite3.IntegrityError):
        svc.db.execute("DELETE FROM audit_events WHERE seq = 1")


def test_audit_chain_detects_tampering_that_bypasses_triggers(svc):
    approved_order(svc)
    svc.db.execute("DROP TRIGGER audit_no_update")
    svc.db.execute("UPDATE audit_events SET payload = '{}' WHERE seq = 3")
    assert svc.audit.verify_chain() == (False, 3)


def test_tool_gateway_allowlist_and_scopes(svc):
    reader = Principal(USER)
    assert svc.tools.call("get_balances", reader, correlation_id="c") == svc.settings.starting_cash
    with pytest.raises(ToolNotAllowed):
        svc.tools.call("submit_order", reader, correlation_id="c")
    svc.tools.register(Tool("place_order", WRITE, "hypothetical write tool", lambda p: "sent"))
    with pytest.raises(ToolNotAllowed):
        svc.tools.call("place_order", reader, correlation_id="c")
    assert [e["event_type"] for e in svc.audit.by_correlation("c")] == [
        "tool.called",
        "tool.denied",
        "tool.denied",
    ]


def test_tool_gateway_broker_handle_cannot_write(svc):
    assert not hasattr(svc.broker_read, "submit_order")
    assert not hasattr(svc.broker_read, "cancel_order")
