"""Unit tests for the European implied volatility solver."""

from __future__ import annotations

import pytest

from options_infra.domain.models import OptionRight
from options_infra.pricing.black_scholes import BSMInput, price_bsm
from options_infra.pricing.iv_solver import solve_implied_volatility


@pytest.mark.parametrize("right", [OptionRight.CALL, OptionRight.PUT])
@pytest.mark.parametrize("strike", [70.0, 90.0, 100.0, 110.0, 135.0])
@pytest.mark.parametrize("true_vol", [0.10, 0.25, 0.60, 1.20])
@pytest.mark.parametrize("years", [0.05, 0.25, 1.5])
def test_iv_round_trip_recovery(
    right: OptionRight,
    strike: float,
    true_vol: float,
    years: float,
) -> None:
    bsm = price_bsm(
        BSMInput(
            right=right,
            spot=100.0,
            strike=strike,
            years=years,
            rate=0.04,
            dividend_yield=0.015,
            volatility=true_vol,
        )
    )
    # Skip effectively zero-time-value wings where option price has < 1e-7 extrinsic
    if bsm.price < 1e-6 or bsm.vega < 1e-6:
        return

    res = solve_implied_volatility(
        market_price=bsm.price,
        right=right,
        spot=100.0,
        strike=strike,
        years=years,
        rate=0.04,
        dividend_yield=0.015,
    )
    assert res.converged is True
    assert res.implied_volatility is not None
    assert res.implied_volatility == pytest.approx(true_vol, abs=1e-5)
    assert res.pricing_error is not None
    assert abs(res.pricing_error) < 1e-6


def test_iv_rejects_no_arbitrage_violations_and_expired() -> None:
    # Deep ITM call priced below intrinsic lower bound
    below = solve_implied_volatility(
        market_price=5.0,
        right=OptionRight.CALL,
        spot=100.0,
        strike=80.0,
        years=0.5,
        rate=0.04,
    )
    assert not below.converged
    assert below.status == "BELOW_INTRINSIC_NO_ARB"

    # Call priced above spot
    above = solve_implied_volatility(
        market_price=105.0,
        right=OptionRight.CALL,
        spot=100.0,
        strike=100.0,
        years=0.5,
        rate=0.04,
    )
    assert not above.converged
    assert above.status == "ABOVE_UPPER_NO_ARB"

    # Expired contract
    expired = solve_implied_volatility(
        market_price=2.0,
        right=OptionRight.CALL,
        spot=100.0,
        strike=100.0,
        years=0.0,
    )
    assert not expired.converged
    assert expired.status == "EXPIRED_OR_ZERO_TIME"
