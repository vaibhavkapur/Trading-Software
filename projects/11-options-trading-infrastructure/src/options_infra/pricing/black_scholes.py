"""Analytic Black-Scholes-Merton pricing and Greeks for European vanilla options.

Explicitly models continuous risk-free rate `r` and continuous dividend yield `q`,
and emits structured warnings when invoked for American-style contracts, near-expiry
pin/gamma risk, or non-zero dividend early-exercise risk.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from datetime import UTC, date, datetime, time

from options_infra.domain.models import (
    ExerciseStyle,
    ModelWarning,
    OptionRight,
    PricingAssumptions,
    PricingModelType,
    WarningSeverity,
)

SQRT_2 = math.sqrt(2.0)
INV_SQRT_2PI = 1.0 / math.sqrt(2.0 * math.pi)
MODEL_VERSION = "bsm-1.0.0"
SECONDS_PER_YEAR = 365.0 * 24.0 * 3600.0


def norm_pdf(x: float) -> float:
    """Standard normal probability density function."""
    return INV_SQRT_2PI * math.exp(-0.5 * x * x)


def norm_cdf(x: float) -> float:
    """Standard normal cumulative distribution function via math.erf."""
    return 0.5 * (1.0 + math.erf(x / SQRT_2))


def time_to_expiry_years(
    expiry: date,
    asof: datetime,
    expiry_cutoff_utc: time = time(20, 0, 0),
) -> float:
    """Compute ACT/365F time to expiry in years, clamped at 0.0 for expired contracts."""
    if asof.tzinfo is None:
        raise ValueError("asof must be timezone-aware UTC")
    expiry_dt = datetime.combine(expiry, expiry_cutoff_utc, tzinfo=UTC)
    delta_seconds = (expiry_dt - asof.astimezone(UTC)).total_seconds()
    return max(0.0, delta_seconds / SECONDS_PER_YEAR)


@dataclass(frozen=True)
class BSMInput:
    right: OptionRight
    spot: float
    strike: float
    years: float
    rate: float
    dividend_yield: float
    volatility: float
    style: ExerciseStyle = ExerciseStyle.EUROPEAN

    def validate(self) -> None:
        if not math.isfinite(self.spot) or self.spot <= 0.0:
            raise ValueError(f"spot must be finite and > 0, got {self.spot}")
        if not math.isfinite(self.strike) or self.strike <= 0.0:
            raise ValueError(f"strike must be finite and > 0, got {self.strike}")
        if not math.isfinite(self.years) or self.years < 0.0:
            raise ValueError(f"years must be finite and >= 0, got {self.years}")
        if not math.isfinite(self.rate):
            raise ValueError(f"rate must be finite, got {self.rate}")
        if not math.isfinite(self.dividend_yield) or self.dividend_yield < 0.0:
            raise ValueError(
                f"dividend_yield must be finite and >= 0, got {self.dividend_yield}"
            )
        if not math.isfinite(self.volatility) or self.volatility < 0.0:
            raise ValueError(f"volatility must be finite and >= 0, got {self.volatility}")


@dataclass(frozen=True)
class BSMResult:
    price: float
    delta: float
    gamma: float
    vega: float
    vega_per_1pct: float
    theta: float
    theta_per_day: float
    rho: float
    d1: float | None
    d2: float | None
    intrinsic_value: float
    time_value: float
    assumptions: PricingAssumptions
    warnings: tuple[ModelWarning, ...]


def _build_warnings(inp: BSMInput) -> tuple[ModelWarning, ...]:
    warnings: list[ModelWarning] = []
    if inp.style == ExerciseStyle.AMERICAN:
        warnings.append(
            ModelWarning(
                code="AMERICAN_PRICED_WITH_EUROPEAN_BSM",
                severity=WarningSeverity.WARNING,
                message=(
                    "Contract is AMERICAN style but priced with European Black-Scholes-Merton. "
                    "Early-exercise premium (especially for ITM puts or calls before ex-dividend) "
                    "is omitted; use BINOMIAL_CRR for American exercise valuation."
                ),
            )
        )
    if inp.dividend_yield > 0.0 and inp.right == OptionRight.CALL:
        warnings.append(
            ModelWarning(
                code="DIVIDEND_EARLY_EXERCISE_RISK",
                severity=WarningSeverity.INFO,
                message=(
                    "Positive dividend yield modeled as continuous yield q. Discrete ex-dividend "
                    "drops can trigger early call exercise on American options."
                ),
            )
        )
    if inp.years == 0.0:
        warnings.append(
            ModelWarning(
                code="AT_EXPIRATION",
                severity=WarningSeverity.INFO,
                message="Option is at or past expiration (T=0); returning intrinsic payoff only.",
            )
        )
    elif inp.years < (7.0 / 365.0):
        warnings.append(
            ModelWarning(
                code="NEAR_EXPIRY_GAMMA_PIN_RISK",
                severity=WarningSeverity.WARNING,
                message=(
                    "Less than 7 calendar days to expiry; Gamma, Theta, and strike pin/assignment "
                    "risk are elevated."
                ),
            )
        )
    if inp.volatility == 0.0 and inp.years > 0.0:
        warnings.append(
            ModelWarning(
                code="ZERO_VOLATILITY_LIMIT",
                severity=WarningSeverity.INFO,
                message="Volatility is zero; returning discounted deterministic forward payoff.",
            )
        )
    return tuple(warnings)


def price_bsm(inp: BSMInput) -> BSMResult:
    """Compute exact Black-Scholes-Merton price and analytic Greeks."""
    inp.validate()
    assumptions = PricingAssumptions(
        model=PricingModelType.BLACK_SCHOLES_MERTON,
        model_version=MODEL_VERSION,
        exercise_style_assumed=ExerciseStyle.EUROPEAN,
        continuous_rate=inp.rate,
        continuous_dividend_yield=inp.dividend_yield,
    )
    warnings = _build_warnings(inp)

    s = inp.spot
    k = inp.strike
    t = inp.years
    r = inp.rate
    q = inp.dividend_yield
    sigma = inp.volatility

    intrinsic = max(s - k, 0.0) if inp.right == OptionRight.CALL else max(k - s, 0.0)

    # At expiration (T == 0)
    if t == 0.0:
        if inp.right == OptionRight.CALL:
            delta = 1.0 if s > k else (0.5 if s == k else 0.0)
        else:
            delta = -1.0 if s < k else (-0.5 if s == k else 0.0)
        return BSMResult(
            price=intrinsic,
            delta=delta,
            gamma=0.0,
            vega=0.0,
            vega_per_1pct=0.0,
            theta=0.0,
            theta_per_day=0.0,
            rho=0.0,
            d1=None,
            d2=None,
            intrinsic_value=intrinsic,
            time_value=0.0,
            assumptions=assumptions,
            warnings=warnings,
        )

    disc_q = math.exp(-q * t)
    disc_r = math.exp(-r * t)

    # Zero volatility deterministic forward limit
    if sigma == 0.0:
        fwd_s = s * disc_q
        disc_k = k * disc_r
        if inp.right == OptionRight.CALL:
            price = max(fwd_s - disc_k, 0.0)
            in_money = fwd_s > disc_k
            delta = disc_q if in_money else 0.0
            theta = (q * fwd_s - r * disc_k) if in_money else 0.0
            rho = (k * t * disc_r) if in_money else 0.0
        else:
            price = max(disc_k - fwd_s, 0.0)
            in_money = disc_k > fwd_s
            delta = -disc_q if in_money else 0.0
            theta = (r * disc_k - q * fwd_s) if in_money else 0.0
            rho = (-k * t * disc_r) if in_money else 0.0
        return BSMResult(
            price=price,
            delta=delta,
            gamma=0.0,
            vega=0.0,
            vega_per_1pct=0.0,
            theta=theta,
            theta_per_day=theta / 365.0,
            rho=rho,
            d1=None,
            d2=None,
            intrinsic_value=intrinsic,
            time_value=price - intrinsic,
            assumptions=assumptions,
            warnings=warnings,
        )

    sqrt_t = math.sqrt(t)
    vol_sqrt_t = sigma * sqrt_t
    d1 = (math.log(s / k) + (r - q + 0.5 * sigma * sigma) * t) / vol_sqrt_t
    d2 = d1 - vol_sqrt_t

    pdf_d1 = norm_pdf(d1)
    cdf_d1 = norm_cdf(d1)
    cdf_d2 = norm_cdf(d2)
    cdf_neg_d1 = norm_cdf(-d1)
    cdf_neg_d2 = norm_cdf(-d2)

    gamma = (disc_q * pdf_d1) / (s * vol_sqrt_t)
    vega = s * disc_q * pdf_d1 * sqrt_t

    if inp.right == OptionRight.CALL:
        price = s * disc_q * cdf_d1 - k * disc_r * cdf_d2
        delta = disc_q * cdf_d1
        theta = (
            -(s * disc_q * pdf_d1 * sigma) / (2.0 * sqrt_t)
            - r * k * disc_r * cdf_d2
            + q * s * disc_q * cdf_d1
        )
        rho = k * t * disc_r * cdf_d2
    else:
        price = k * disc_r * cdf_neg_d2 - s * disc_q * cdf_neg_d1
        delta = -disc_q * cdf_neg_d1
        theta = (
            -(s * disc_q * pdf_d1 * sigma) / (2.0 * sqrt_t)
            + r * k * disc_r * cdf_neg_d2
            - q * s * disc_q * cdf_neg_d1
        )
        rho = -k * t * disc_r * cdf_neg_d2

    # Guard tiny negative floating-point dust deep OTM
    price = max(0.0, price)

    return BSMResult(
        price=price,
        delta=delta,
        gamma=gamma,
        vega=vega,
        vega_per_1pct=vega / 100.0,
        theta=theta,
        theta_per_day=theta / 365.0,
        rho=rho,
        d1=d1,
        d2=d2,
        intrinsic_value=intrinsic,
        time_value=price - intrinsic,
        assumptions=assumptions,
        warnings=warnings,
    )


def put_call_parity_residual(
    spot: float,
    strike: float,
    years: float,
    rate: float,
    dividend_yield: float,
    volatility: float,
) -> float:
    """Return (C - P) - (S*e^(-qT) - K*e^(-rT)); should be ~0 within float precision."""
    call = price_bsm(
        BSMInput(
            right=OptionRight.CALL,
            spot=spot,
            strike=strike,
            years=years,
            rate=rate,
            dividend_yield=dividend_yield,
            volatility=volatility,
        )
    )
    put = price_bsm(
        BSMInput(
            right=OptionRight.PUT,
            spot=spot,
            strike=strike,
            years=years,
            rate=rate,
            dividend_yield=dividend_yield,
            volatility=volatility,
        )
    )
    lhs = call.price - put.price
    rhs = spot * math.exp(-dividend_yield * years) - strike * math.exp(-rate * years)
    return lhs - rhs


def finite_difference_greeks(
    inp: BSMInput,
    spot_bump_rel: float = 1e-4,
    vol_bump_abs: float = 1e-4,
    time_bump_years: float = 1e-5,
    rate_bump_abs: float = 1e-4,
) -> dict[str, float]:
    """Compute central finite-difference Greeks to cross-verify analytic derivatives."""
    inp.validate()
    ds = inp.spot * spot_bump_rel
    p_up = price_bsm(
        BSMInput(**{**inp.__dict__, "spot": inp.spot + ds})
    ).price
    p_mid = price_bsm(inp).price
    p_dn = price_bsm(
        BSMInput(**{**inp.__dict__, "spot": inp.spot - ds})
    ).price

    delta = (p_up - p_dn) / (2.0 * ds)
    gamma = (p_up - 2.0 * p_mid + p_dn) / (ds * ds)

    dv = vol_bump_abs
    v_up = price_bsm(
        BSMInput(**{**inp.__dict__, "volatility": inp.volatility + dv})
    ).price
    v_dn = price_bsm(
        BSMInput(**{**inp.__dict__, "volatility": max(0.0, inp.volatility - dv)})
    ).price
    vega = (v_up - v_dn) / (inp.volatility + dv - max(0.0, inp.volatility - dv))

    dt = min(time_bump_years, inp.years * 0.5) if inp.years > 0 else 0.0
    if dt > 0.0:
        t_up = price_bsm(
            BSMInput(**{**inp.__dict__, "years": inp.years + dt})
        ).price
        t_dn = price_bsm(
            BSMInput(**{**inp.__dict__, "years": inp.years - dt})
        ).price
        # Theta is -dV/dT
        theta = -(t_up - t_dn) / (2.0 * dt)
    else:
        theta = 0.0

    dr = rate_bump_abs
    r_up = price_bsm(
        BSMInput(**{**inp.__dict__, "rate": inp.rate + dr})
    ).price
    r_dn = price_bsm(
        BSMInput(**{**inp.__dict__, "rate": inp.rate - dr})
    ).price
    rho = (r_up - r_dn) / (2.0 * dr)

    return {
        "delta": delta,
        "gamma": gamma,
        "vega": vega,
        "theta": theta,
        "rho": rho,
    }
