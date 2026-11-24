"""Signed, expiring approvals bound to exact order parameters.

The signature is an HMAC over the approval's fields, including the proposal hash. A
row forged or edited in the database fails verification because the secret never
leaves the process.
"""

from __future__ import annotations

import hashlib
import hmac
from datetime import datetime, timedelta

from .domain import Approval, Decision, TradeProposal, hash_of
from .ids import IdFactory


class ApprovalInvalid(Exception):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code


class ApprovalService:
    def __init__(self, secret: bytes, ttl_seconds: int, new_id: IdFactory) -> None:
        self._secret = secret
        self._ttl = timedelta(seconds=ttl_seconds)
        self._new_id = new_id

    def _signature(self, a: Approval | dict) -> str:
        f = a.model_dump(mode="json") if isinstance(a, Approval) else a
        message = "|".join(
            [
                f["id"],
                f["proposal_id"],
                f["proposal_hash"],
                f["approver_id"],
                f["decision"],
                f["created_at"],
                f["expires_at"],
            ]
        )
        return hmac.new(self._secret, message.encode(), hashlib.sha256).hexdigest()

    def issue(
        self, proposal: TradeProposal, approver_id: str, decision: Decision, now: datetime
    ) -> Approval:
        approval = Approval(
            id=self._new_id("apr"),
            proposal_id=proposal.id,
            proposal_hash=hash_of(proposal),
            approver_id=approver_id,
            decision=decision,
            created_at=now,
            expires_at=now + self._ttl,
            signature="",
        )
        return approval.model_copy(update={"signature": self._signature(approval)})

    def verify(self, approval: Approval, proposal: TradeProposal, now: datetime) -> None:
        """Raise unless this approval authorises executing `proposal` right now."""
        if not hmac.compare_digest(approval.signature, self._signature(approval)):
            raise ApprovalInvalid(
                "APPROVAL_SIGNATURE_INVALID", "approval signature does not verify"
            )
        if approval.decision is not Decision.APPROVE:
            raise ApprovalInvalid("NOT_APPROVED", "the proposal was not approved")
        if approval.proposal_id != proposal.id or approval.approver_id != proposal.user_id:
            raise ApprovalInvalid("APPROVAL_MISMATCH", "approval does not belong to this proposal")
        if approval.proposal_hash != hash_of(proposal):
            raise ApprovalInvalid(
                "PROPOSAL_CHANGED", "order parameters differ from what was approved"
            )
        if now >= approval.expires_at:
            raise ApprovalInvalid(
                "APPROVAL_EXPIRED", f"approval expired at {approval.expires_at.isoformat()}"
            )
