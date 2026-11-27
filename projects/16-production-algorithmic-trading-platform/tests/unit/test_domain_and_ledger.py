"""Unit tests for canonical domain invariants, live enablement gate, and double-entry ledger."""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from decimal import Decimal

import pytest

from trading_platform.domain.config import (
    ExecutionPolicyConfig,
    validate_live_enablement,
)
from trading_platform.domain.errors import LiveModeBlockedError
from trading_platform.domain.events import (
    ExecutionMode,
    FillEvent,
    JournalLine,
    LedgerEntry,
    LiveApprovalRecord,
    LiveRolloutBundle,
    MarketTick,
    OrderIntent,
    OrderType,
    Role,
    Side,
)
from trading_platform.ledger.ledger_service import LedgerService
from trading_platform.storage.sqlite_store import SQLiteStore


def test_market_tick_rejects_naive_timestamp_and_crossed_book() -> None:
    now = datetime(2026, 11, 27, 12, 0, 0, tzinfo=UTC)
    with pytest.raises(ValueError, match="timezone-aware"):
        MarketTick(
            tick_id="T1",
            feed_id="F1",
            symbol="AAPL",
            bid=Decimal("100"),
            ask=Decimal("101"),
            last=Decimal("100.5"),
            sequence_no=1,
            event_time_utc=datetime(2026, 11, 27, 12, 0, 0),
            available_time_utc=now,
        )
    with pytest.raises(ValueError, match="Crossed book"):
        MarketTick(
            tick_id="T2",
            feed_id="F1",
            symbol="AAPL",
            bid=Decimal("102"),
            ask=Decimal("101"),
            last=Decimal("101.5"),
            sequence_no=1,
            event_time_utc=now,
            available_time_utc=now,
        )


def test_order_intent_limit_price_and_fingerprint_invariants() -> None:
    with pytest.raises(ValueError, match="limit_price"):
        OrderIntent(
            idempotency_key="IDEM-1",
            client_order_id="ORD-1",
            account_id="A1",
            strategy_id="S1",
            symbol="AAPL",
            side=Side.BUY,
            order_type=OrderType.LIMIT,
            quantity=Decimal("10"),
            limit_price=None,
            reference_price=Decimal("190"),
        )

    i1 = OrderIntent(
        idempotency_key="IDEM-1",
        client_order_id="ORD-1",
        account_id="A1",
        strategy_id="S1",
        symbol="AAPL",
        side=Side.BUY,
        order_type=OrderType.LIMIT,
        quantity=Decimal("10"),
        limit_price=Decimal("190.50"),
        reference_price=Decimal("190.00"),
    )
    i2 = i1.model_copy(update={"quantity": Decimal("20")})
    assert i1.payload_fingerprint() != i2.payload_fingerprint()


def test_ledger_entry_enforces_strict_double_entry_balance() -> None:
    with pytest.raises(ValueError, match="Unbalanced ledger entry"):
        LedgerEntry(
            entry_id="JNL-BAD",
            idempotency_key="K-BAD",
            account_id="A1",
            entry_type="TEST",
            reference_id="R1",
            description="Unbalanced entry",
            lines=[
                JournalLine(account_code="1000-CASH", debit=Decimal("100")),
                JournalLine(account_code="3000-EQUITY-CAPITAL", credit=Decimal("90")),
            ],
        )


def test_ledger_long_short_flip_and_idempotent_fill_posting() -> None:
    store = SQLiteStore(":memory:")
    ledger = LedgerService(store, account_id="ACCT-1")
    ledger.initialize_capital(Decimal("100000"))

    f_buy = FillEvent(
        fill_id="F1",
        client_order_id="O1",
        broker_order_id="B1",
        account_id="ACCT-1",
        strategy_id="S1",
        symbol="AAPL",
        side=Side.BUY,
        quantity=Decimal("100"),
        price=Decimal("100.00"),
        commission=Decimal("1.00"),
    )
    e1, pos1 = ledger.post_fill(f_buy)
    assert e1 is not None
    assert pos1.quantity == Decimal("100")
    assert ledger.cash_balance() == Decimal("89999.0000")

    # Duplicate fill is idempotently ignored
    e_dup, pos_dup = ledger.post_fill(f_buy)
    assert e_dup is None
    assert pos_dup.quantity == Decimal("100")

    # Sell 150 shares @ 110 -> closes 100 long (+1000 realized PnL) and opens 50 short
    f_sell = FillEvent(
        fill_id="F2",
        client_order_id="O2",
        broker_order_id="B2",
        account_id="ACCT-1",
        strategy_id="S1",
        symbol="AAPL",
        side=Side.SELL,
        quantity=Decimal("150"),
        price=Decimal("110.00"),
        commission=Decimal("1.50"),
    )
    e2, pos2 = ledger.post_fill(f_sell)
    assert e2 is not None
    assert pos2.quantity == Decimal("-50")
    assert pos2.average_cost == Decimal("110.00")
    assert pos2.realized_pnl == Decimal("1000.0000")
    assert ledger.is_balanced()
    assert ledger.total_realized_pnl() == Decimal("1000.0000")
    assert ledger.total_commissions() == Decimal("2.5000")
    store.close()


def test_live_mode_blocked_by_default_and_requires_full_approval_bundle() -> None:
    # Default policy is PAPER and blocks live enablement
    default_policy = ExecutionPolicyConfig()
    assert default_policy.execution_mode == ExecutionMode.PAPER
    assert not default_policy.allow_live_execution

    with pytest.raises(LiveModeBlockedError) as exc_info:
        ExecutionPolicyConfig(execution_mode=ExecutionMode.LIVE)
    assert len(exc_info.value.details["gaps"]) >= 2

    # Even with allow_live_execution=True, missing approvals or separation of duties fails closed
    now = datetime(2026, 11, 27, 12, 0, 0, tzinfo=UTC)
    valid_bundle = LiveRolloutBundle(
        bundle_id="LIVE-2026-001",
        venue_readiness_verified=True,
        regulatory_readiness_verified=True,
        independent_risk_review_ref="RISK-REV-001",
        security_review_ref="SEC-REV-001",
        soak_report_ref="SOAK-001",
        approvals=[
            LiveApprovalRecord(
                approver_id="alice-risk",
                role=Role.RISK_OFFICER,
                approval_ref="APP-1",
                signed_at_utc=now - timedelta(days=1),
                expires_at_utc=now + timedelta(days=30),
                attestation="Risk limits verified",
            ),
            LiveApprovalRecord(
                approver_id="bob-comp",
                role=Role.COMPLIANCE,
                approval_ref="APP-2",
                signed_at_utc=now - timedelta(days=1),
                expires_at_utc=now + timedelta(days=30),
                attestation="Regulatory readiness confirmed",
            ),
            LiveApprovalRecord(
                approver_id="carol-cto",
                role=Role.CTO,
                approval_ref="APP-3",
                signed_at_utc=now - timedelta(days=1),
                expires_at_utc=now + timedelta(days=30),
                attestation="Engineering signoff",
            ),
        ],
    )
    approved_policy = ExecutionPolicyConfig(
        execution_mode=ExecutionMode.LIVE,
        allow_live_execution=True,
        live_rollout_bundle=valid_bundle,
    )
    assert validate_live_enablement(approved_policy, now_utc=now) == []

    # Expired approval must fail closed
    with pytest.raises(LiveModeBlockedError):
        validate_live_enablement(approved_policy, now_utc=now + timedelta(days=60))
