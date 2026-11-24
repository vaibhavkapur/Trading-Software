"""Cox-Ross-Rubinstein (CRR) binomial tree pricer supporting American & European exercise.

Provided as a stretch-goal valuation kernel to quantify American early-exercise
premiums that European Black-Scholes-Merton intentionally omits.
"""

from __future__ import annotations

import math
from dataclasses import dataclass

from options_infra.domain.models import (
    ExerciseStyle,
    ModelWarning,
    OptionRight,
    PricingAssumptions,
    PricingModelType,
    WarningSeverity,
)


@dataclass(frozen=True)
class BinomialResult:
    price: float
    european_reference_price: float
    early_exercise_premium: float
    delta: float
    gamma: float
    theta: float
    steps: int
    assumptions: PricingAssumptions
    warnings: tuple[ModelWarning, ...]


def _tree_price(
    right: OptionRight,
    spot: float,
    strike: float,
    years: float,
    rate: float,
    dividend_yield: float,
    volatility: float,
    style: ExerciseStyle,
    steps: int,
) -> tuple[float, float, float, float]:
    """Return (price, delta, gamma, theta_per_year) from a CRR recombining tree."""
    if years == 0.0:
        intrinsic = max(spot - strike, 0.0) if right == OptionRight.CALL else max(strike - spot, 0.0)
        if right == OptionRight.CALL:
            delta = 1.0 if spot > strike else (0.5 if spot == strike else 0.0)
        else:
            delta = -1.0 if spot < strike else (-0.5 if spot == strike else 0.0)
        return intrinsic, delta, 0.0, 0.0

    dt = years / steps
    u = math.exp(volatility * math.sqrt(dt))
    d = 1.0 / u
    disc = math.exp(-rate * dt)
    growth = math.exp((rate - dividend_yield) * dt)
    p = (growth - d) / (u - d)
    if not (0.0 <= p <= 1.0):
        raise ValueError(
            f"CRR tree risk-neutral probability out of [0, 1] (p={p:.4f}); increase steps"
        )
    q_prob = 1.0 - p

    # Terminal payoffs
    values = [0.0] * (steps + 1)
    for i in range(steps + 1):
        s_t = spot * (u ** i) * (d ** (steps - i))
        values[i] = max(s_t - strike, 0.0) if right == OptionRight.CALL else max(strike - s_t, 0.0)

    v20 = v21 = v22 = 0.0
    v10 = v11 = 0.0

    for step in range(steps - 1, -1, -1):
        next_values = [0.0] * (step + 1)
        for i in range(step + 1):
            cont = disc * (p * values[i + 1] + q_prob * values[i])
            if style == ExerciseStyle.AMERICAN:
                s_node = spot * (u ** i) * (d ** (step - i))
                ex = (
                    max(s_node - strike, 0.0)
                    if right == OptionRight.CALL
                    else max(strike - s_node, 0.0)
                )
                next_values[i] = max(cont, ex)
            else:
                next_values[i] = cont
        values = next_values
        if step == 2:
            v20, v21, v22 = values[0], values[1], values[2]
        elif step == 1:
            v10, v11 = values[0], values[1]

    price = values[0]
    if steps >= 2:
        s_u = spot * u
        s_d = spot * d
        delta = (v11 - v10) / (s_u - s_d)
        s_uu = spot * u * u
        s_dd = spot * d * d
        delta_u = (v22 - v21) / (s_uu - spot)
        delta_d = (v21 - v20) / (spot - s_dd)
        gamma = (delta_u - delta_d) / (0.5 * (s_uu - s_dd))
        theta = (v21 - price) / (2.0 * dt)
    else:
        delta = gamma = theta = 0.0

    return price, delta, gamma, theta


def price_binomial_crr(
    right: OptionRight,
    spot: float,
    strike: float,
    years: float,
    rate: float,
    dividend_yield: float,
    volatility: float,
    style: ExerciseStyle = ExerciseStyle.AMERICAN,
    steps: int = 200,
) -> BinomialResult:
    """Price an American or European option using a CRR binomial tree."""
    if spot <= 0 or strike <= 0 or years < 0 or volatility <= 0 or steps < 2:
        raise ValueError("Invalid inputs for CRR binomial tree")

    price, delta, gamma, theta = _tree_price(
        right=right,
        spot=spot,
        strike=strike,
        years=years,
        rate=rate,
        dividend_yield=dividend_yield,
        volatility=volatility,
        style=style,
        steps=steps,
    )
    if style == ExerciseStyle.AMERICAN:
        eu_price, _, _, _ = _tree_price(
            right=right,
            spot=spot,
            strike=strike,
            years=years,
            rate=rate,
            dividend_yield=dividend_yield,
            volatility=volatility,
            style=ExerciseStyle.EUROPEAN,
            steps=steps,
        )
    else:
        eu_price = price

    early_premium = max(0.0, price - eu_price)
    warnings: list[ModelWarning] = []
    if style == ExerciseStyle.AMERICAN and early_premium > 1e-4:
        warnings.append(
            ModelWarning(
                code="EARLY_EXERCISE_PREMIUM_DETECTED",
                severity=WarningSeverity.WARNING,
                message=(
                    f"American early-exercise premium estimated at {early_premium:.4f} over "
                    f"European equivalent; short holders face early-assignment risk."
                ),
            )
        )

    assumptions = PricingAssumptions(
        model=PricingModelType.BINOMIAL_CRR,
        model_version=f"crr-n{steps}",
        exercise_style_assumed=style,
        continuous_rate=rate,
        continuous_dividend_yield=dividend_yield,
        volatility_assumption=f"Constant volatility CRR recombining lattice ({steps} steps)",
        early_exercise_limitation=(
            "CRR lattice captures continuous-yield early exercise, but discrete dividend "
            "schedules and borrow-rate spikes require explicit event-date modeling."
        ),
    )

    return BinomialResult(
        price=price,
        european_reference_price=eu_price,
        early_exercise_premium=early_premium,
        delta=delta,
        gamma=gamma,
        theta=theta,
        steps=steps,
        assumptions=assumptions,
        warnings=tuple(warnings),
    )
