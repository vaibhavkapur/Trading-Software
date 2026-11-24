"""Implied volatility solver for European Black-Scholes-Merton prices.

Uses safeguarded Newton-Raphson with automatic fallback to bisection/bracketed search,
enforcing European no-arbitrage bounds before attempting inversion.
"""

from __future__ import annotations

import math
from dataclasses import dataclass

from options_infra.domain.models import OptionRight
from options_infra.pricing.black_scholes import BSMInput, price_bsm


@dataclass(frozen=True)
class IVResult:
    converged: bool
    implied_volatility: float | None
    iterations: int
    pricing_error: float | None
    status: str
    message: str


def _no_arb_bounds(
    right: OptionRight,
    spot: float,
    strike: float,
    years: float,
    rate: float,
    dividend_yield: float,
) -> tuple[float, float]:
    disc_s = spot * math.exp(-dividend_yield * years)
    disc_k = strike * math.exp(-rate * years)
    if right == OptionRight.CALL:
        lower = max(disc_s - disc_k, 0.0)
        upper = disc_s
    else:
        lower = max(disc_k - disc_s, 0.0)
        upper = disc_k
    return lower, upper


def solve_implied_volatility(
    market_price: float,
    right: OptionRight,
    spot: float,
    strike: float,
    years: float,
    rate: float = 0.04,
    dividend_yield: float = 0.0,
    tol: float = 1e-8,
    max_iterations: int = 100,
    min_vol: float = 1e-6,
    max_vol: float = 10.0,
) -> IVResult:
    """Invert European BSM price to implied volatility with no-arbitrage checks."""
    if not math.isfinite(market_price) or market_price < 0.0:
        return IVResult(
            converged=False,
            implied_volatility=None,
            iterations=0,
            pricing_error=None,
            status="INVALID_PRICE",
            message=f"Market price must be finite and >= 0, got {market_price}",
        )
    if not math.isfinite(spot) or spot <= 0.0 or not math.isfinite(strike) or strike <= 0.0:
        return IVResult(
            converged=False,
            implied_volatility=None,
            iterations=0,
            pricing_error=None,
            status="INVALID_INPUT",
            message="Spot and strike must be finite and > 0",
        )
    if not math.isfinite(years) or years <= 0.0:
        return IVResult(
            converged=False,
            implied_volatility=None,
            iterations=0,
            pricing_error=None,
            status="EXPIRED_OR_ZERO_TIME",
            message="Cannot invert implied volatility when years <= 0",
        )

    lower, upper = _no_arb_bounds(right, spot, strike, years, rate, dividend_yield)
    eps = 1e-10
    if market_price < lower - eps:
        return IVResult(
            converged=False,
            implied_volatility=None,
            iterations=0,
            pricing_error=market_price - lower,
            status="BELOW_INTRINSIC_NO_ARB",
            message=(
                f"Market price {market_price:.6f} is below European no-arbitrage lower bound "
                f"{lower:.6f}"
            ),
        )
    if market_price > upper + eps:
        return IVResult(
            converged=False,
            implied_volatility=None,
            iterations=0,
            pricing_error=market_price - upper,
            status="ABOVE_UPPER_NO_ARB",
            message=(
                f"Market price {market_price:.6f} exceeds European no-arbitrage upper bound "
                f"{upper:.6f}"
            ),
        )
    if abs(market_price - lower) <= eps:
        return IVResult(
            converged=False,
            implied_volatility=None,
            iterations=0,
            pricing_error=0.0,
            status="ZERO_TIME_VALUE",
            message="Market price equals discounted intrinsic value (zero time value)",
        )

    def eval_diff(vol: float) -> tuple[float, float]:
        res = price_bsm(
            BSMInput(
                right=right,
                spot=spot,
                strike=strike,
                years=years,
                rate=rate,
                dividend_yield=dividend_yield,
                volatility=vol,
            )
        )
        return res.price - market_price, res.vega

    low = min_vol
    high = max_vol
    f_low, _ = eval_diff(low)
    f_high, _ = eval_diff(high)

    # Expand upper bracket if extreme high-vol quote is still above model price
    while f_high < 0.0 and high < 50.0:
        high *= 2.0
        f_high, _ = eval_diff(high)

    if f_low > 0.0:
        return IVResult(
            converged=False,
            implied_volatility=None,
            iterations=0,
            pricing_error=f_low,
            status="BELOW_MIN_VOL_BRACKET",
            message="Price is below minimum searchable volatility bracket",
        )
    if f_high < 0.0:
        return IVResult(
            converged=False,
            implied_volatility=None,
            iterations=0,
            pricing_error=f_high,
            status="ABOVE_MAX_VOL_BRACKET",
            message=f"Implied volatility exceeds maximum bracket ({high:.1f})",
        )

    # Manaster-Koehler initial guess clamped safely inside [low, high]
    fwd = spot * math.exp((rate - dividend_yield) * years)
    mk_guess = math.sqrt(2.0 * abs(math.log(fwd / strike)) / years) if fwd != strike else 0.25
    sigma = min(max(mk_guess, 0.05), min(2.0, high * 0.5))
    if not (low < sigma < high):
        sigma = 0.5 * (low + high)

    for iteration in range(1, max_iterations + 1):
        diff, vega = eval_diff(sigma)
        vol_err_ok = (vega > 1e-12 and abs(diff / vega) <= 1e-7) or (high - low) <= 1e-7
        if (abs(diff) <= tol and vol_err_ok) or (high - low) <= 1e-9:
            return IVResult(
                converged=True,
                implied_volatility=sigma,
                iterations=iteration,
                pricing_error=diff,
                status="CONVERGED",
                message="Implied volatility converged",
            )

        if diff < 0.0:
            low = sigma
        else:
            high = sigma

        # Safeguarded Newton step; fall back to bisection if vega tiny or outside bracket
        if vega > 1e-12:
            newton_next = sigma - diff / vega
            if low < newton_next < high:
                sigma = newton_next
                continue
        sigma = 0.5 * (low + high)

    final_diff, _ = eval_diff(sigma)
    return IVResult(
        converged=abs(final_diff) <= tol * 10.0,
        implied_volatility=sigma if abs(final_diff) <= tol * 10.0 else None,
        iterations=max_iterations,
        pricing_error=final_diff,
        status="MAX_ITERATIONS_REACHED",
        message=f"Did not converge within {max_iterations} iterations (err={final_diff:.2e})",
    )
