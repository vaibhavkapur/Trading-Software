"""Defined-risk vertical spread builder, payoff profile, and paper order preview."""

from __future__ import annotations

from datetime import UTC, datetime
from decimal import ROUND_HALF_UP, Decimal
from typing import Literal

from pydantic import BaseModel, Field

from options_infra.domain.models import (
    ExerciseStyle,
    ModelWarning,
    OptionRight,
    OrderLeg,
    OrderSide,
    PricingAssumptions,
    PricingModelType,
    SpreadType,
    WarningSeverity,
)
from options_infra.instruments.repository import InstrumentRepository
from options_infra.pricing.black_scholes import BSMInput, price_bsm, time_to_expiry_years
from options_infra.risk.aggregator import compute_contract_greeks


class SpreadValidationError(ValueError):
    """Raised when two legs do not form a valid defined-risk vertical spread."""


class PayoffPoint(BaseModel):
    spot: Decimal
    expiry_pnl: Decimal
    t0_theoretical_pnl: float
    net_delta: float


class VerticalSpreadPreview(BaseModel):
    execution_mode: Literal["PAPER_SIMULATED"] = "PAPER_SIMULATED"
    spread_type: SpreadType
    underlying: str
    expiry: str
    right: OptionRight
    style: ExerciseStyle
    multiplier: int
    quantity: int
    pricing_mode: Literal["MID", "NATURAL"]
    legs: list[OrderLeg]
    long_strike: Decimal
    short_strike: Decimal
    width: Decimal
    spot: Decimal
    net_debit_credit_per_unit: Decimal = Field(
        ..., description="Per-spread price: >0 net debit paid, <0 net credit received"
    )
    net_debit_credit: Decimal = Field(
        ..., description="Total currency impact: >0 debit paid, <0 credit received"
    )
    max_loss_estimate: Decimal
    max_gain_estimate: Decimal
    breakeven_spot: Decimal
    risk_reward_ratio: float | None
    net_delta: float
    net_gamma: float
    net_vega_per_1pct: float
    net_theta_per_day: float
    payoff_curve: list[PayoffPoint]
    assumptions: PricingAssumptions
    limitations: list[str]
    warnings: list[ModelWarning]
    rule_version: str = "strategy-rules-1.0.0"
    previewed_at: datetime


def _money(value: Decimal) -> Decimal:
    return value.quantize(Decimal("0.01"), rounding=ROUND_HALF_UP)


def _classify_vertical(
    right: OptionRight, long_strike: Decimal, short_strike: Decimal
) -> SpreadType:
    if right == OptionRight.CALL:
        return (
            SpreadType.BULL_CALL_DEBIT
            if long_strike < short_strike
            else SpreadType.BEAR_CALL_CREDIT
        )
    return (
        SpreadType.BEAR_PUT_DEBIT
        if long_strike > short_strike
        else SpreadType.BULL_PUT_CREDIT
    )


def _expiry_intrinsic(right: OptionRight, spot: Decimal, strike: Decimal) -> Decimal:
    if right == OptionRight.CALL:
        return max(spot - strike, Decimal("0"))
    return max(strike - spot, Decimal("0"))


def build_vertical_spread_preview(
    long_contract_id: str,
    short_contract_id: str,
    quantity: int,
    repository: InstrumentRepository,
    pricing_mode: Literal["MID", "NATURAL"] = "MID",
    valuation_time: datetime | None = None,
    enforce_freshness: bool = False,
    payoff_points: int = 25,
) -> VerticalSpreadPreview:
    """Construct a defined-risk vertical spread preview with exact Decimal risk math."""
    if quantity <= 0:
        raise SpreadValidationError(f"Spread quantity must be > 0, got {quantity}")
    if long_contract_id == short_contract_id:
        raise SpreadValidationError("Long and short legs must reference distinct contracts")

    long_c = repository.get_contract(long_contract_id)
    short_c = repository.get_contract(short_contract_id)

    if long_c.underlying != short_c.underlying:
        raise SpreadValidationError(
            "Vertical spread legs must share underlying "
            f"({long_c.underlying} != {short_c.underlying})"
        )
    if long_c.expiry != short_c.expiry:
        raise SpreadValidationError(
            f"Vertical spread legs must share expiry ({long_c.expiry} != {short_c.expiry})"
        )
    if long_c.right != short_c.right:
        raise SpreadValidationError(
            f"Vertical spread legs must share option right ({long_c.right} != {short_c.right})"
        )
    if long_c.multiplier != short_c.multiplier:
        raise SpreadValidationError("Vertical spread legs must share contract multiplier")
    if long_c.strike == short_c.strike:
        raise SpreadValidationError("Vertical spread legs must have distinct strikes")

    now_utc = (valuation_time or repository.latest_quote_time() or datetime.now(UTC)).astimezone(
        UTC
    )
    long_q = repository.get_quote(
        long_contract_id,
        reference_now=now_utc,
        enforce_freshness=enforce_freshness,
    )
    short_q = repository.get_quote(
        short_contract_id,
        reference_now=now_utc,
        enforce_freshness=enforce_freshness,
    )

    spread_type = _classify_vertical(long_c.right, long_c.strike, short_c.strike)
    long_px = long_q.ask if pricing_mode == "NATURAL" else long_q.mid
    short_px = short_q.bid if pricing_mode == "NATURAL" else short_q.mid

    # Positive = net debit paid; negative = net credit received
    net_per_unit = (long_px - short_px).quantize(Decimal("0.0001"), rounding=ROUND_HALF_UP)
    qty_mult = Decimal(quantity * long_c.multiplier)
    net_total = _money(net_per_unit * qty_mult)
    width = abs(long_c.strike - short_c.strike)

    warnings: list[ModelWarning] = []
    warnings.extend(repository.inspect_quote_health(long_q, reference_now=now_utc))
    warnings.extend(repository.inspect_quote_health(short_q, reference_now=now_utc))

    if spread_type in (SpreadType.BULL_CALL_DEBIT, SpreadType.BEAR_PUT_DEBIT):
        max_loss_unit = net_per_unit
        max_gain_unit = width - net_per_unit
        breakeven = (
            long_c.strike + net_per_unit
            if long_c.right == OptionRight.CALL
            else long_c.strike - net_per_unit
        )
        if net_per_unit >= width:
            warnings.append(
                ModelWarning(
                    code="DEBIT_EXCEEDS_SPREAD_WIDTH",
                    severity=WarningSeverity.CRITICAL,
                    message=(
                        f"Net debit ({net_per_unit}) >= spread width ({width}); "
                        "spread has non-positive maximum gain at expiration."
                    ),
                )
            )
    else:
        credit_unit = -net_per_unit
        max_gain_unit = credit_unit
        max_loss_unit = width - credit_unit
        breakeven = (
            short_c.strike + credit_unit
            if long_c.right == OptionRight.CALL
            else short_c.strike - credit_unit
        )
        if credit_unit <= Decimal("0"):
            warnings.append(
                ModelWarning(
                    code="CREDIT_SPREAD_HAS_NET_DEBIT",
                    severity=WarningSeverity.CRITICAL,
                    message=(
                        "Credit spread structure priced at a net debit under current quotes; "
                        "verify bid/ask liquidity before paper submission."
                    ),
                )
            )
        elif credit_unit >= width:
            warnings.append(
                ModelWarning(
                    code="CREDIT_EXCEEDS_SPREAD_WIDTH",
                    severity=WarningSeverity.WARNING,
                    message=(
                        f"Net credit ({credit_unit}) >= spread width ({width}); quotes may be "
                        "stale or crossed relative to no-arbitrage parity."
                    ),
                )
            )

    max_loss_total = _money(max(Decimal("0.00"), max_loss_unit * qty_mult))
    max_gain_total = _money(max(Decimal("0.00"), max_gain_unit * qty_mult))
    risk_reward = (
        round(float(max_gain_total / max_loss_total), 4)
        if max_loss_total > Decimal("0")
        else None
    )

    # Compute Greeks & implied volatilities for T+0 curve
    long_snap, long_vol, long_warns = compute_contract_greeks(long_c, long_q, now_utc)
    short_snap, short_vol, short_warns = compute_contract_greeks(short_c, short_q, now_utc)

    scale = float(quantity * long_c.multiplier)
    net_delta = (long_snap.delta - short_snap.delta) * scale
    net_gamma = (long_snap.gamma - short_snap.gamma) * scale
    net_vega_1pct = (long_snap.vega_per_1pct - short_snap.vega_per_1pct) * scale
    net_theta_day = (long_snap.theta_per_day - short_snap.theta_per_day) * scale

    for w in long_warns + short_warns:
        if all(existing.code != w.code for existing in warnings):
            warnings.append(w)

    # Always attach pin/assignment & exercise limitations
    warnings.append(
        ModelWarning(
            code="PIN_AND_ASSIGNMENT_RISK",
            severity=WarningSeverity.INFO,
            message=(
                f"Finishing between strikes ({min(long_c.strike, short_c.strike)} and "
                f"{max(long_c.strike, short_c.strike)}) at expiration exposes the short leg "
                f"({short_c.id}) to pin and unhedged post-expiry assignment risk."
            ),
        )
    )
    if long_c.style == ExerciseStyle.AMERICAN or short_c.style == ExerciseStyle.AMERICAN:
        warnings.append(
            ModelWarning(
                code="AMERICAN_SPREAD_EARLY_ASSIGNMENT",
                severity=WarningSeverity.WARNING,
                message=(
                    "Contracts are AMERICAN style; short leg may be assigned early (e.g., ahead "
                    "of ex-dividend or when deep ITM), breaking the spread's defined-risk profile "
                    "temporarily if unmonitored."
                ),
            )
        )

    spot = long_q.underlying_px
    years = time_to_expiry_years(long_c.expiry, now_utc)
    rate_f = float(long_q.rate)
    div_f = float(long_q.dividend_yield)

    low_bound = min(float(spot) * 0.75, float(min(long_c.strike, short_c.strike)) * 0.85)
    high_bound = max(float(spot) * 1.25, float(max(long_c.strike, short_c.strike)) * 1.15)
    step = (high_bound - low_bound) / max(1, payoff_points - 1)

    grid_spots = {
        Decimal(f"{low_bound + i * step:.2f}") for i in range(payoff_points)
    }
    grid_spots.add(long_c.strike.quantize(Decimal("0.01")))
    grid_spots.add(short_c.strike.quantize(Decimal("0.01")))
    grid_spots.add(spot.quantize(Decimal("0.01")))
    if Decimal("0") < breakeven < Decimal(f"{high_bound * 1.5:.2f}"):
        grid_spots.add(breakeven.quantize(Decimal("0.01"), rounding=ROUND_HALF_UP))

    payoff_curve: list[PayoffPoint] = []
    entry_theo_unit = long_snap.theoretical_price - short_snap.theoretical_price

    for s_dec in sorted(grid_spots):
        long_exp = _expiry_intrinsic(long_c.right, s_dec, long_c.strike)
        short_exp = _expiry_intrinsic(short_c.right, s_dec, short_c.strike)
        exp_pnl = _money(((long_exp - short_exp) - net_per_unit) * qty_mult)

        s_f = float(s_dec)
        l_bsm = price_bsm(
            BSMInput(
                right=long_c.right,
                spot=s_f,
                strike=float(long_c.strike),
                years=years,
                rate=rate_f,
                dividend_yield=div_f,
                volatility=long_vol,
                style=long_c.style,
            )
        )
        s_bsm = price_bsm(
            BSMInput(
                right=short_c.right,
                spot=s_f,
                strike=float(short_c.strike),
                years=years,
                rate=rate_f,
                dividend_yield=div_f,
                volatility=short_vol,
                style=short_c.style,
            )
        )
        t0_pnl = ((l_bsm.price - s_bsm.price) - entry_theo_unit) * scale
        pt_delta = (l_bsm.delta - s_bsm.delta) * scale
        payoff_curve.append(
            PayoffPoint(
                spot=s_dec,
                expiry_pnl=exp_pnl,
                t0_theoretical_pnl=round(t0_pnl, 2),
                net_delta=round(pt_delta, 4),
            )
        )

    assumptions = PricingAssumptions(
        model=PricingModelType.BLACK_SCHOLES_MERTON,
        exercise_style_assumed=ExerciseStyle.EUROPEAN,
        continuous_rate=rate_f,
        continuous_dividend_yield=div_f,
    )
    limitations = [
        f"T+0 curve assumes constant per-leg IV (long={long_vol:.2%}, short={short_vol:.2%}), "
        f"continuous r={rate_f:.2%}, q={div_f:.2%}, European BSM.",
        "Expiration payoff holds only if both legs are held to expiry without early assignment, "
        "margin liquidation, or pin-risk exercise imbalance.",
        f"Entry pricing uses simulated {pricing_mode} quotes; commissions, exchange fees, and "
        "slippage beyond quoted spread are excluded.",
    ]

    legs = [
        OrderLeg(
            contract_id=long_c.id,
            side=OrderSide.BUY,
            ratio=1,
            limit_price=long_px.quantize(long_c.tick_size, rounding=ROUND_HALF_UP),
        ),
        OrderLeg(
            contract_id=short_c.id,
            side=OrderSide.SELL,
            ratio=1,
            limit_price=short_px.quantize(short_c.tick_size, rounding=ROUND_HALF_UP),
        ),
    ]

    return VerticalSpreadPreview(
        spread_type=spread_type,
        underlying=long_c.underlying,
        expiry=long_c.expiry.isoformat(),
        right=long_c.right,
        style=long_c.style,
        multiplier=long_c.multiplier,
        quantity=quantity,
        pricing_mode=pricing_mode,
        legs=legs,
        long_strike=long_c.strike,
        short_strike=short_c.strike,
        width=width,
        spot=spot,
        net_debit_credit_per_unit=net_per_unit,
        net_debit_credit=net_total,
        max_loss_estimate=max_loss_total,
        max_gain_estimate=max_gain_total,
        breakeven_spot=breakeven.quantize(Decimal("0.01"), rounding=ROUND_HALF_UP),
        risk_reward_ratio=risk_reward,
        net_delta=round(net_delta, 4),
        net_gamma=round(net_gamma, 6),
        net_vega_per_1pct=round(net_vega_1pct, 4),
        net_theta_per_day=round(net_theta_day, 4),
        payoff_curve=payoff_curve,
        assumptions=assumptions,
        limitations=limitations,
        warnings=warnings,
        previewed_at=now_utc,
    )
