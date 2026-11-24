"""Unit tests for portfolio Greek aggregation, shock scenarios, and vertical spreads."""

from __future__ import annotations

from datetime import date
from decimal import Decimal

import pytest

from options_infra.domain.models import (
    ExerciseStyle,
    OptionContract,
    OptionPosition,
    OptionRight,
    SpreadType,
)
from options_infra.instruments.repository import InstrumentRepository
from options_infra.instruments.simulator import DEFAULT_ASOF
from options_infra.risk.aggregator import aggregate_portfolio_greeks
from options_infra.risk.scenarios import run_portfolio_scenarios
from options_infra.strategy.vertical_spread import (
    SpreadValidationError,
    build_vertical_spread_preview,
)


def test_portfolio_greeks_respect_direction_quantity_and_multiplier() -> None:
    repo = InstrumentRepository.from_default_simulator(asof=DEFAULT_ASOF)
    cid = "XYZ-20270115-100-C"

    long_1 = aggregate_portfolio_greeks(
        positions=[OptionPosition(contract_id=cid, quantity=1, avg_price=Decimal("6.00"))],
        repository=repo,
        valuation_time=DEFAULT_ASOF,
    )
    short_3 = aggregate_portfolio_greeks(
        positions=[OptionPosition(contract_id=cid, quantity=-3, avg_price=Decimal("6.00"))],
        repository=repo,
        valuation_time=DEFAULT_ASOF,
    )

    assert short_3.net_delta == pytest.approx(-3.0 * long_1.net_delta, abs=1e-9)
    assert short_3.net_gamma == pytest.approx(-3.0 * long_1.net_gamma, abs=1e-9)
    assert short_3.net_vega == pytest.approx(-3.0 * long_1.net_vega, abs=1e-9)
    assert short_3.net_theta == pytest.approx(-3.0 * long_1.net_theta, abs=1e-9)

    # Verify contract multiplier scaling (default 100 vs custom 10)
    mini_contract = OptionContract(
        id="XYZ-20270115-100-C-MINI",
        underlying="XYZ",
        expiry=date(2027, 1, 15),
        strike=Decimal("100.00"),
        right=OptionRight.CALL,
        multiplier=10,
        style=ExerciseStyle.EUROPEAN,
    )
    repo.upsert_contract(mini_contract)
    base_quote = repo.get_quote(cid)
    repo.upsert_quote(
        base_quote.model_copy(update={"contract_id": mini_contract.id, "sequence_id": 9999})
    )
    mini_long = aggregate_portfolio_greeks(
        positions=[
            OptionPosition(contract_id=mini_contract.id, quantity=1, avg_price=Decimal("6.00"))
        ],
        repository=repo,
        valuation_time=DEFAULT_ASOF,
    )
    assert long_1.net_delta == pytest.approx(10.0 * mini_long.net_delta, abs=1e-9)
    assert long_1.net_gamma == pytest.approx(10.0 * mini_long.net_gamma, abs=1e-9)

    # Opposite identical positions neutralize Greeks
    flat = aggregate_portfolio_greeks(
        positions=[
            OptionPosition(contract_id=cid, quantity=2, avg_price=Decimal("6.00")),
            OptionPosition(contract_id=cid, quantity=-2, avg_price=Decimal("6.00")),
        ],
        repository=repo,
        valuation_time=DEFAULT_ASOF,
    )
    assert flat.net_delta == pytest.approx(0.0, abs=1e-9)
    assert flat.net_gamma == pytest.approx(0.0, abs=1e-9)
    assert flat.net_vega == pytest.approx(0.0, abs=1e-9)


def test_scenario_grid_zero_shock_is_zero_pnl_and_matches_direction() -> None:
    repo = InstrumentRepository.from_default_simulator(asof=DEFAULT_ASOF)
    positions = [
        OptionPosition(contract_id="XYZ-20270115-100-C", quantity=2, avg_price=Decimal("6.00")),
    ]
    report = run_portfolio_scenarios(
        positions=positions,
        repository=repo,
        spot_shocks_pct=[-0.10, 0.0, 0.10],
        vol_shocks_abs=[-0.05, 0.0, 0.05],
        days_forward=0,
        valuation_time=DEFAULT_ASOF,
    )
    by_key = {(c.spot_shock_pct, c.vol_shock_abs): c for c in report.cells}
    assert by_key[(0.0, 0.0)].scenario_pnl == pytest.approx(0.0, abs=1e-9)
    # Long call gains on spot up and vol up
    assert by_key[(0.10, 0.0)].scenario_pnl > 0.0
    assert by_key[(-0.10, 0.0)].scenario_pnl < 0.0
    assert by_key[(0.0, 0.05)].scenario_pnl > 0.0
    assert by_key[(0.0, -0.05)].scenario_pnl < 0.0


@pytest.mark.parametrize(
    ("long_id", "short_id", "expected_type"),
    [
        ("XYZ-20270115-100-C", "XYZ-20270115-105-C", SpreadType.BULL_CALL_DEBIT),
        ("XYZ-20270115-105-C", "XYZ-20270115-100-C", SpreadType.BEAR_CALL_CREDIT),
        ("XYZ-20270115-95-P", "XYZ-20270115-100-P", SpreadType.BULL_PUT_CREDIT),
        ("XYZ-20270115-100-P", "XYZ-20270115-95-P", SpreadType.BEAR_PUT_DEBIT),
    ],
)
def test_all_four_vertical_spreads_exact_decimal_bounds_and_assumptions(
    long_id: str,
    short_id: str,
    expected_type: SpreadType,
) -> None:
    repo = InstrumentRepository.from_default_simulator(asof=DEFAULT_ASOF)
    preview = build_vertical_spread_preview(
        long_contract_id=long_id,
        short_contract_id=short_id,
        quantity=3,
        repository=repo,
        pricing_mode="MID",
        valuation_time=DEFAULT_ASOF,
    )
    assert preview.spread_type == expected_type
    assert preview.max_loss_estimate > Decimal("0")
    assert preview.max_gain_estimate > Decimal("0")
    # Exact defined-risk invariant: max_gain + max_loss == width * quantity * multiplier
    total_width_value = preview.width * Decimal(preview.quantity * preview.multiplier)
    assert preview.max_gain_estimate + preview.max_loss_estimate == total_width_value

    # Payoff curve endpoints match exact max gain and -max loss
    exp_pnls = [pt.expiry_pnl for pt in preview.payoff_curve]
    assert max(exp_pnls) == preview.max_gain_estimate
    assert min(exp_pnls) == -preview.max_loss_estimate

    # Definition of Done: every payoff graph identifies pricing assumptions & limitations
    assert preview.assumptions.model.value == "BLACK_SCHOLES_MERTON"
    assert "early-exercise" in preview.assumptions.early_exercise_limitation.lower()
    assert len(preview.limitations) >= 2
    assert any(w.code == "PIN_AND_ASSIGNMENT_RISK" for w in preview.warnings)


def test_vertical_spread_rejects_mismatched_legs() -> None:
    repo = InstrumentRepository.from_default_simulator(asof=DEFAULT_ASOF)
    # Mixed rights (call + put)
    with pytest.raises(SpreadValidationError):
        build_vertical_spread_preview(
            long_contract_id="XYZ-20270115-100-C",
            short_contract_id="XYZ-20270115-105-P",
            quantity=1,
            repository=repo,
        )
    # Mixed expiries
    with pytest.raises(SpreadValidationError):
        build_vertical_spread_preview(
            long_contract_id="XYZ-20270115-100-C",
            short_contract_id="XYZ-20270319-105-C",
            quantity=1,
            repository=repo,
        )
    # Mixed underlyings
    with pytest.raises(SpreadValidationError):
        build_vertical_spread_preview(
            long_contract_id="XYZ-20270115-100-C",
            short_contract_id="ABC-20270115-150-C",
            quantity=1,
            repository=repo,
        )
