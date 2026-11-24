"""Unit tests for Black-Scholes-Merton pricing, put-call parity, Greeks, and boundaries."""

from __future__ import annotations

import math

import pytest

from options_infra.domain.models import ExerciseStyle, OptionRight
from options_infra.pricing.black_scholes import (
    BSMInput,
    finite_difference_greeks,
    price_bsm,
    put_call_parity_residual,
)


def test_bsm_reference_values_no_dividend() -> None:
    """Verify against textbook BSM reference: S=100, K=105, T=0.25, r=0.04, q=0, sigma=0.30."""
    call = price_bsm(
        BSMInput(
            right=OptionRight.CALL,
            spot=100.0,
            strike=105.0,
            years=0.25,
            rate=0.04,
            dividend_yield=0.0,
            volatility=0.30,
        )
    )
    put = price_bsm(
        BSMInput(
            right=OptionRight.PUT,
            spot=100.0,
            strike=105.0,
            years=0.25,
            rate=0.04,
            dividend_yield=0.0,
            volatility=0.30,
        )
    )
    # Verified analytical values for these exact parameters
    assert call.price == pytest.approx(4.316879, abs=1e-5)
    assert put.price == pytest.approx(8.272107, abs=1e-5)
    assert call.delta == pytest.approx(0.427207, abs=1e-5)
    assert put.delta == pytest.approx(call.delta - 1.0, abs=1e-12)
    assert call.gamma == pytest.approx(put.gamma, abs=1e-12)
    assert call.vega == pytest.approx(put.vega, abs=1e-12)


@pytest.mark.parametrize("spot", [75.0, 100.0, 130.0])
@pytest.mark.parametrize("strike", [80.0, 100.0, 120.0])
@pytest.mark.parametrize("years", [0.05, 0.25, 1.0, 2.0])
@pytest.mark.parametrize("rate", [-0.01, 0.0, 0.05])
@pytest.mark.parametrize("div_yield", [0.0, 0.025])
@pytest.mark.parametrize("vol", [0.10, 0.30, 0.75])
def test_put_call_parity_holds_within_numerical_tolerance(
    spot: float,
    strike: float,
    years: float,
    rate: float,
    div_yield: float,
    vol: float,
) -> None:
    residual = put_call_parity_residual(
        spot=spot,
        strike=strike,
        years=years,
        rate=rate,
        dividend_yield=div_yield,
        volatility=vol,
    )
    assert abs(residual) < 1e-11


@pytest.mark.parametrize("right", [OptionRight.CALL, OptionRight.PUT])
@pytest.mark.parametrize("spot", [85.0, 100.0, 118.0])
@pytest.mark.parametrize("div_yield", [0.0, 0.03])
def test_analytic_greeks_match_finite_difference(
    right: OptionRight,
    spot: float,
    div_yield: float,
) -> None:
    inp = BSMInput(
        right=right,
        spot=spot,
        strike=100.0,
        years=0.5,
        rate=0.045,
        dividend_yield=div_yield,
        volatility=0.28,
    )
    analytic = price_bsm(inp)
    numeric = finite_difference_greeks(inp)

    assert analytic.delta == pytest.approx(numeric["delta"], abs=1e-5)
    assert analytic.gamma == pytest.approx(numeric["gamma"], abs=1e-5)
    assert analytic.vega == pytest.approx(numeric["vega"], abs=1e-4)
    assert analytic.theta == pytest.approx(numeric["theta"], abs=1e-3)
    assert analytic.rho == pytest.approx(numeric["rho"], abs=1e-4)


def test_boundary_conditions_expiry_and_zero_vol() -> None:
    at_exp_itm = price_bsm(
        BSMInput(
            right=OptionRight.CALL,
            spot=110.0,
            strike=100.0,
            years=0.0,
            rate=0.04,
            dividend_yield=0.0,
            volatility=0.30,
        )
    )
    assert at_exp_itm.price == pytest.approx(10.0)
    assert at_exp_itm.delta == 1.0
    assert at_exp_itm.gamma == 0.0
    assert any(w.code == "AT_EXPIRATION" for w in at_exp_itm.warnings)

    zero_vol_put = price_bsm(
        BSMInput(
            right=OptionRight.PUT,
            spot=90.0,
            strike=100.0,
            years=1.0,
            rate=0.05,
            dividend_yield=0.0,
            volatility=0.0,
        )
    )
    expected = max(100.0 * math.exp(-0.05) - 90.0, 0.0)
    assert zero_vol_put.price == pytest.approx(expected, abs=1e-12)
    assert any(w.code == "ZERO_VOLATILITY_LIMIT" for w in zero_vol_put.warnings)


def test_american_and_near_expiry_warnings_emitted() -> None:
    res = price_bsm(
        BSMInput(
            right=OptionRight.CALL,
            spot=100.0,
            strike=100.0,
            years=3.0 / 365.0,
            rate=0.04,
            dividend_yield=0.02,
            volatility=0.25,
            style=ExerciseStyle.AMERICAN,
        )
    )
    codes = {w.code for w in res.warnings}
    assert "AMERICAN_PRICED_WITH_EUROPEAN_BSM" in codes
    assert "NEAR_EXPIRY_GAMMA_PIN_RISK" in codes
    assert "DIVIDEND_EARLY_EXERCISE_RISK" in codes


@pytest.mark.parametrize(
    "bad_kwargs",
    [
        {"spot": 0.0},
        {"spot": -10.0},
        {"strike": -1.0},
        {"years": -0.1},
        {"volatility": -0.2},
        {"dividend_yield": -0.01},
        {"spot": float("nan")},
    ],
)
def test_invalid_inputs_rejected(bad_kwargs: dict[str, float]) -> None:
    base = {
        "right": OptionRight.CALL,
        "spot": 100.0,
        "strike": 100.0,
        "years": 0.25,
        "rate": 0.04,
        "dividend_yield": 0.0,
        "volatility": 0.25,
    }
    base.update(bad_kwargs)
    with pytest.raises(ValueError):
        price_bsm(BSMInput(**base))  # type: ignore[arg-type]
