from __future__ import annotations

from datetime import UTC, datetime

import pytest

from atp.clock import FixedClock
from atp.config import Settings
from atp.ids import SequentialIds
from atp.planner import RuleBasedPlanner
from atp.service import TradingService

# Monday 2026-01-05 10:00 New York time: inside the regular session.
SESSION_OPEN_UTC = datetime(2026, 1, 5, 15, 0, 0, tzinfo=UTC)
USER = "alice"


def make_service(clock=None, db_path=":memory:", planner=None, **overrides) -> TradingService:
    settings = Settings(
        db_path=db_path, approval_secret=b"test-secret", planner="rules", **overrides
    )
    return TradingService(
        settings,
        clock=clock or FixedClock(SESSION_OPEN_UTC),
        new_id=SequentialIds(),
        planner=planner or RuleBasedPlanner(),
    )


@pytest.fixture
def clock() -> FixedClock:
    return FixedClock(SESSION_OPEN_UTC)


@pytest.fixture
def svc(clock) -> TradingService:
    return make_service(clock)


def approved_order(svc: TradingService, text: str = "Buy $500 of SPY using a limit order"):
    """Drive a proposal through evaluate + approve; return (proposal, order)."""
    _, proposal = svc.create_intent(USER, text)
    decision = svc.evaluate(USER, proposal.id)
    assert decision.passed, decision.violations
    _, order = svc.decide(USER, proposal.id, proposal.proposal_hash, "approve")
    return svc.store.proposal(proposal.id), order
