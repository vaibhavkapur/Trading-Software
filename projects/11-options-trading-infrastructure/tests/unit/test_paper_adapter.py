"""Unit tests for broker constraints, pre-trade risk gate, and paper order adapter."""

from __future__ import annotations

from decimal import Decimal

import pytest

from options_infra.domain.models import OrderLeg, OrderSide, OrderStatus
from options_infra.instruments.repository import InstrumentRepository
from options_infra.instruments.simulator import DEFAULT_ASOF
from options_infra.paper_adapter.adapter import PaperMultiLegAdapter
from options_infra.paper_adapter.broker_rules import BrokerCapabilities
from options_infra.paper_adapter.pre_trade_risk import PreTradeRiskConfig
from options_infra.paper_adapter.store import PaperOrderStore


def _make_adapter(
    capabilities: BrokerCapabilities | None = None,
    risk_config: PreTradeRiskConfig | None = None,
) -> PaperMultiLegAdapter:
    repo = InstrumentRepository.from_default_simulator(asof=DEFAULT_ASOF)
    store = PaperOrderStore(":memory:")
    return PaperMultiLegAdapter(
        repository=repo,
        store=store,
        capabilities=capabilities,
        risk_config=risk_config,
    )


def test_paper_order_valid_vertical_spread_fills_and_updates_positions() -> None:
    adapter = _make_adapter()
    legs = [
        OrderLeg(contract_id="XYZ-20270115-100-C", side=OrderSide.BUY, ratio=1),
        OrderLeg(contract_id="XYZ-20270115-105-C", side=OrderSide.SELL, ratio=1),
    ]
    order, dedup = adapter.submit_order(
        legs=legs,
        quantity=2,
        strategy_type="BULL_CALL_DEBIT",
        idempotency_key="unit-fill-001",
        asof=DEFAULT_ASOF,
    )
    assert not dedup
    assert order.execution_mode == "PAPER_SIMULATED"
    assert order.status == OrderStatus.FILLED
    assert order.max_loss_estimate > Decimal("0")
    assert order.max_gain_estimate is not None
    assert order.max_loss_estimate + order.max_gain_estimate == Decimal("1000.00")

    positions = {p.contract_id: p for p in adapter.store.list_positions()}
    assert positions["XYZ-20270115-100-C"].quantity == 2
    assert positions["XYZ-20270115-105-C"].quantity == -2


def test_broker_rejects_naked_short_and_unsupported_multi_leg() -> None:
    # Uncovered short rejected under require_defined_risk=True
    adapter = _make_adapter()
    order, _ = adapter.submit_order(
        legs=[OrderLeg(contract_id="XYZ-20270115-105-C", side=OrderSide.SELL, ratio=1)],
        quantity=1,
        asof=DEFAULT_ASOF,
    )
    assert order.status == OrderStatus.REJECTED
    assert order.rejection_reason is not None
    assert "BROKER_CONSTRAINT_FAILED" in order.rejection_reason
    assert "defined-risk" in order.rejection_reason

    # Broker with supports_multi_leg=False rejects 2-leg spread
    single_leg_broker = _make_adapter(
        capabilities=BrokerCapabilities(supports_multi_leg=False)
    )
    order2, _ = single_leg_broker.submit_order(
        legs=[
            OrderLeg(contract_id="XYZ-20270115-100-C", side=OrderSide.BUY, ratio=1),
            OrderLeg(contract_id="XYZ-20270115-105-C", side=OrderSide.SELL, ratio=1),
        ],
        quantity=1,
        asof=DEFAULT_ASOF,
    )
    assert order2.status == OrderStatus.REJECTED
    assert "does not support multi-leg" in (order2.rejection_reason or "")


def test_broker_rejects_invalid_tick_size() -> None:
    adapter = _make_adapter()
    order, _ = adapter.submit_order(
        legs=[
            OrderLeg(
                contract_id="XYZ-20270115-100-C",
                side=OrderSide.BUY,
                ratio=1,
                limit_price=Decimal("5.005"),
            ),
            OrderLeg(contract_id="XYZ-20270115-105-C", side=OrderSide.SELL, ratio=1),
        ],
        quantity=1,
        asof=DEFAULT_ASOF,
    )
    assert order.status == OrderStatus.REJECTED
    assert "tick size" in (order.rejection_reason or "")


def test_pre_trade_risk_limits_and_kill_switch() -> None:
    strict_adapter = _make_adapter(
        risk_config=PreTradeRiskConfig(max_loss_per_order=Decimal("100.00"))
    )
    legs = [
        OrderLeg(contract_id="XYZ-20270115-100-C", side=OrderSide.BUY, ratio=1),
        OrderLeg(contract_id="XYZ-20270115-105-C", side=OrderSide.SELL, ratio=1),
    ]
    order, _ = strict_adapter.submit_order(legs=legs, quantity=5, asof=DEFAULT_ASOF)
    assert order.status == OrderStatus.REJECTED
    assert "PRE_TRADE_RISK_REJECTED" in (order.rejection_reason or "")

    killed_adapter = _make_adapter(
        risk_config=PreTradeRiskConfig(kill_switch_active=True)
    )
    order2, _ = killed_adapter.submit_order(legs=legs, quantity=1, asof=DEFAULT_ASOF)
    assert order2.status == OrderStatus.REJECTED
    assert "kill switch" in (order2.rejection_reason or "")


def test_resting_order_lifecycle_cancel_and_terminal_guard() -> None:
    adapter = _make_adapter()
    legs = [
        OrderLeg(contract_id="XYZ-20270115-100-C", side=OrderSide.BUY, ratio=1),
        OrderLeg(contract_id="XYZ-20270115-105-C", side=OrderSide.SELL, ratio=1),
    ]
    resting, _ = adapter.submit_order(
        legs=legs,
        quantity=1,
        limit_net_debit_credit=Decimal("0.10"),  # Unmarketable limit -> SUBMITTED
        asof=DEFAULT_ASOF,
    )
    assert resting.status == OrderStatus.SUBMITTED
    assert adapter.store.list_positions() == []

    cancelled = adapter.cancel_order(resting.id, asof=DEFAULT_ASOF)
    assert cancelled.status == OrderStatus.CANCELLED

    # Terminal state cannot be cancelled again
    with pytest.raises(ValueError, match="terminal status"):
        adapter.cancel_order(resting.id, asof=DEFAULT_ASOF)
