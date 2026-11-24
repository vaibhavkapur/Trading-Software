"""Unit tests for the stretch-goal CRR binomial tree American/European pricer."""

from __future__ import annotations

import pytest

from options_infra.domain.models import ExerciseStyle, OptionRight
from options_infra.pricing.binomial import price_binomial_crr
from options_infra.pricing.black_scholes import BSMInput, price_bsm


def test_european_binomial_converges_to_bsm() -> None:
    bsm = price_bsm(
        BSMInput(
            right=OptionRight.PUT,
            spot=100.0,
            strike=105.0,
            years=0.5,
            rate=0.04,
            dividend_yield=0.01,
            volatility=0.30,
        )
    )
    crr = price_binomial_crr(
        right=OptionRight.PUT,
        spot=100.0,
        strike=105.0,
        years=0.5,
        rate=0.04,
        dividend_yield=0.01,
        volatility=0.30,
        style=ExerciseStyle.EUROPEAN,
        steps=300,
    )
    assert crr.price == pytest.approx(bsm.price, abs=0.02)
    assert crr.delta == pytest.approx(bsm.delta, abs=0.01)
    assert crr.early_exercise_premium == 0.0


def test_american_put_has_positive_early_exercise_premium() -> None:
    crr = price_binomial_crr(
        right=OptionRight.PUT,
        spot=85.0,
        strike=100.0,
        years=1.0,
        rate=0.06,
        dividend_yield=0.0,
        volatility=0.25,
        style=ExerciseStyle.AMERICAN,
        steps=200,
    )
    assert crr.price > crr.european_reference_price
    assert crr.early_exercise_premium > 0.25
    assert any(w.code == "EARLY_EXERCISE_PREMIUM_DETECTED" for w in crr.warnings)


def test_american_call_without_dividend_matches_european() -> None:
    crr = price_binomial_crr(
        right=OptionRight.CALL,
        spot=110.0,
        strike=100.0,
        years=1.0,
        rate=0.05,
        dividend_yield=0.0,
        volatility=0.25,
        style=ExerciseStyle.AMERICAN,
        steps=200,
    )
    assert crr.early_exercise_premium == pytest.approx(0.0, abs=1e-9)
