"""Portfolio Greek aggregation incorporating position direction, quantity, and multipliers."""

from __future__ import annotations

from datetime import UTC, datetime
from decimal import ROUND_HALF_UP, Decimal

from pydantic import BaseModel, Field

from options_infra.domain.models import (
    ExerciseStyle,
    GreekSnapshot,
    ModelWarning,
    OptionContract,
    OptionPosition,
    OptionQuote,
    PricingAssumptions,
    PricingModelType,
    WarningSeverity,
)
from options_infra.instruments.repository import InstrumentRepository
from options_infra.pricing.black_scholes import BSMInput, price_bsm, time_to_expiry_years
from options_infra.pricing.iv_solver import solve_implied_volatility


class PositionRiskDetail(BaseModel):
    contract_id: str
    underlying: str
    right: str
    strike: Decimal
    expiry: str
    style: ExerciseStyle
    multiplier: int
    quantity: int
    avg_price: Decimal
    mark_price: Decimal
    underlying_px: Decimal
    implied_volatility: float | None
    unit_delta: float
    unit_gamma: float
    unit_vega: float
    unit_theta_per_day: float
    position_delta: float
    dollar_delta: float
    position_gamma: float
    position_vega: float
    position_vega_per_1pct: float
    position_theta: float
    position_theta_per_day: float
    position_rho: float
    market_value: Decimal
    cost_basis: Decimal
    unrealized_pnl: Decimal
    warnings: list[ModelWarning] = Field(default_factory=list)


class UnderlyingExposure(BaseModel):
    underlying: str
    spot: Decimal
    position_count: int
    net_delta: float
    dollar_delta: float
    net_gamma: float
    net_vega_per_1pct: float
    net_theta_per_day: float
    market_value: Decimal
    unrealized_pnl: Decimal


class PortfolioGreeksSummary(BaseModel):
    asof: datetime
    positions: list[PositionRiskDetail]
    by_underlying: list[UnderlyingExposure]
    net_delta: float
    dollar_delta: float
    net_gamma: float
    net_vega: float
    net_vega_per_1pct: float
    net_theta: float
    net_theta_per_day: float
    net_rho: float
    total_market_value: Decimal
    total_cost_basis: Decimal
    total_unrealized_pnl: Decimal
    assumptions: PricingAssumptions
    warnings: list[ModelWarning] = Field(default_factory=list)


def _money(val: Decimal) -> Decimal:
    return val.quantize(Decimal("0.01"), rounding=ROUND_HALF_UP)


def compute_contract_greeks(
    contract: OptionContract,
    quote: OptionQuote,
    valuation_time: datetime | None = None,
    fallback_vol: float = 0.25,
) -> tuple[GreekSnapshot, float, list[ModelWarning]]:
    """Invert IV from quote mid and evaluate BSM Greeks for a contract."""
    asof_ref = (valuation_time or quote.asof).astimezone(UTC)
    years = time_to_expiry_years(contract.expiry, asof_ref)
    spot = float(quote.underlying_px)
    strike = float(contract.strike)
    rate = float(quote.rate)
    div = float(quote.dividend_yield)
    mid = float(quote.mid)

    warnings: list[ModelWarning] = []
    iv_res = solve_implied_volatility(
        market_price=mid,
        right=contract.right,
        spot=spot,
        strike=strike,
        years=years,
        rate=rate,
        dividend_yield=div,
    )
    if iv_res.converged and iv_res.implied_volatility is not None:
        used_vol = iv_res.implied_volatility
        iv_value: float | None = iv_res.implied_volatility
    else:
        used_vol = fallback_vol
        iv_value = None
        warnings.append(
            ModelWarning(
                code=f"IV_FALLBACK_{iv_res.status}",
                severity=WarningSeverity.WARNING,
                message=(
                    f"IV inversion did not converge for {contract.id} ({iv_res.message}); "
                    f"using fallback volatility {fallback_vol:.2f} for Greeks."
                ),
            )
        )

    bsm = price_bsm(
        BSMInput(
            right=contract.right,
            spot=spot,
            strike=strike,
            years=years,
            rate=rate,
            dividend_yield=div,
            volatility=used_vol,
            style=contract.style,
        )
    )
    warnings.extend(bsm.warnings)

    snapshot = GreekSnapshot(
        contract_id=contract.id,
        iv=iv_value,
        theoretical_price=bsm.price,
        delta=bsm.delta,
        gamma=bsm.gamma,
        vega=bsm.vega,
        vega_per_1pct=bsm.vega_per_1pct,
        theta=bsm.theta,
        theta_per_day=bsm.theta_per_day,
        rho=bsm.rho,
        model=PricingModelType.BLACK_SCHOLES_MERTON,
        warnings=warnings,
        computed_at=asof_ref,
    )
    return snapshot, used_vol, warnings


def aggregate_portfolio_greeks(
    positions: list[OptionPosition],
    repository: InstrumentRepository,
    valuation_time: datetime | None = None,
    vol_overrides: dict[str, float] | None = None,
    enforce_freshness: bool = False,
) -> PortfolioGreeksSummary:
    """Aggregate Greeks and market valuation across positions with direction and multipliers."""
    details: list[PositionRiskDetail] = []
    all_warnings: list[ModelWarning] = []
    seen_warning_keys: set[tuple[str, str]] = set()
    by_underlying_acc: dict[str, dict[str, object]] = {}

    now_utc = (valuation_time or repository.latest_quote_time() or datetime.now(UTC)).astimezone(
        UTC
    )

    net_delta = 0.0
    dollar_delta = 0.0
    net_gamma = 0.0
    net_vega = 0.0
    net_vega_1pct = 0.0
    net_theta = 0.0
    net_theta_day = 0.0
    net_rho = 0.0
    total_mv = Decimal("0.00")
    total_cost = Decimal("0.00")
    total_pnl = Decimal("0.00")

    sample_rate = 0.04
    sample_div = 0.0

    for pos in positions:
        contract = repository.get_contract(pos.contract_id)
        quote = repository.get_quote(
            pos.contract_id,
            reference_now=now_utc,
            enforce_freshness=enforce_freshness,
        )
        sample_rate = float(quote.rate)
        sample_div = float(quote.dividend_yield)

        pos_warnings = repository.inspect_quote_health(quote, reference_now=now_utc)
        if vol_overrides and pos.contract_id in vol_overrides:
            override_vol = vol_overrides[pos.contract_id]
            years = time_to_expiry_years(contract.expiry, now_utc)
            bsm = price_bsm(
                BSMInput(
                    right=contract.right,
                    spot=float(quote.underlying_px),
                    strike=float(contract.strike),
                    years=years,
                    rate=float(quote.rate),
                    dividend_yield=float(quote.dividend_yield),
                    volatility=override_vol,
                    style=contract.style,
                )
            )
            snapshot = GreekSnapshot(
                contract_id=contract.id,
                iv=override_vol,
                theoretical_price=bsm.price,
                delta=bsm.delta,
                gamma=bsm.gamma,
                vega=bsm.vega,
                vega_per_1pct=bsm.vega_per_1pct,
                theta=bsm.theta,
                theta_per_day=bsm.theta_per_day,
                rho=bsm.rho,
                warnings=list(bsm.warnings),
                computed_at=now_utc,
            )
            pos_warnings.extend(bsm.warnings)
        else:
            snapshot, _, calc_warnings = compute_contract_greeks(
                contract=contract,
                quote=quote,
                valuation_time=now_utc,
            )
            pos_warnings.extend(calc_warnings)

        # Signed scale = direction & quantity * contract multiplier
        scale = float(pos.quantity * contract.multiplier)
        spot_f = float(quote.underlying_px)

        p_delta = snapshot.delta * scale
        p_dollar_delta = p_delta * spot_f
        p_gamma = snapshot.gamma * scale
        p_vega = snapshot.vega * scale
        p_vega_1pct = snapshot.vega_per_1pct * scale
        p_theta = snapshot.theta * scale
        p_theta_day = snapshot.theta_per_day * scale
        p_rho = snapshot.rho * scale

        mark = quote.mid
        qty_mult = Decimal(pos.quantity * contract.multiplier)
        mv = _money(mark * qty_mult)
        cost = _money(pos.avg_price * qty_mult)
        upnl = _money((mark - pos.avg_price) * qty_mult)

        net_delta += p_delta
        dollar_delta += p_dollar_delta
        net_gamma += p_gamma
        net_vega += p_vega
        net_vega_1pct += p_vega_1pct
        net_theta += p_theta
        net_theta_day += p_theta_day
        net_rho += p_rho
        total_mv += mv
        total_cost += cost
        total_pnl += upnl

        if pos.quantity < 0:
            short_warn = ModelWarning(
                code="SHORT_OPTION_ASSIGNMENT_RISK",
                severity=WarningSeverity.INFO,
                message=(
                    f"Short position ({pos.quantity}) in {contract.id} carries assignment and "
                    "expiration pin risk if the underlying finishes near or beyond strike."
                ),
            )
            pos_warnings.append(short_warn)

        for w in pos_warnings:
            key = (w.code, w.message)
            if key not in seen_warning_keys:
                seen_warning_keys.add(key)
                all_warnings.append(w)

        details.append(
            PositionRiskDetail(
                contract_id=contract.id,
                underlying=contract.underlying,
                right=contract.right.value,
                strike=contract.strike,
                expiry=contract.expiry.isoformat(),
                style=contract.style,
                multiplier=contract.multiplier,
                quantity=pos.quantity,
                avg_price=pos.avg_price,
                mark_price=mark.quantize(Decimal("0.0001")),
                underlying_px=quote.underlying_px,
                implied_volatility=snapshot.iv,
                unit_delta=snapshot.delta,
                unit_gamma=snapshot.gamma,
                unit_vega=snapshot.vega,
                unit_theta_per_day=snapshot.theta_per_day,
                position_delta=p_delta,
                dollar_delta=p_dollar_delta,
                position_gamma=p_gamma,
                position_vega=p_vega,
                position_vega_per_1pct=p_vega_1pct,
                position_theta=p_theta,
                position_theta_per_day=p_theta_day,
                position_rho=p_rho,
                market_value=mv,
                cost_basis=cost,
                unrealized_pnl=upnl,
                warnings=pos_warnings,
            )
        )

        bucket = by_underlying_acc.setdefault(
            contract.underlying,
            {
                "spot": quote.underlying_px,
                "count": 0,
                "delta": 0.0,
                "dollar_delta": 0.0,
                "gamma": 0.0,
                "vega_1pct": 0.0,
                "theta_day": 0.0,
                "mv": Decimal("0.00"),
                "pnl": Decimal("0.00"),
            },
        )
        bucket["count"] = int(bucket["count"]) + 1
        bucket["delta"] = float(bucket["delta"]) + p_delta
        bucket["dollar_delta"] = float(bucket["dollar_delta"]) + p_dollar_delta
        bucket["gamma"] = float(bucket["gamma"]) + p_gamma
        bucket["vega_1pct"] = float(bucket["vega_1pct"]) + p_vega_1pct
        bucket["theta_day"] = float(bucket["theta_day"]) + p_theta_day
        bucket["mv"] = Decimal(bucket["mv"]) + mv
        bucket["pnl"] = Decimal(bucket["pnl"]) + upnl

    by_underlying = [
        UnderlyingExposure(
            underlying=u,
            spot=Decimal(data["spot"]),
            position_count=int(data["count"]),
            net_delta=float(data["delta"]),
            dollar_delta=float(data["dollar_delta"]),
            net_gamma=float(data["gamma"]),
            net_vega_per_1pct=float(data["vega_1pct"]),
            net_theta_per_day=float(data["theta_day"]),
            market_value=_money(Decimal(data["mv"])),
            unrealized_pnl=_money(Decimal(data["pnl"])),
        )
        for u, data in sorted(by_underlying_acc.items())
    ]

    assumptions = PricingAssumptions(
        model=PricingModelType.BLACK_SCHOLES_MERTON,
        continuous_rate=sample_rate,
        continuous_dividend_yield=sample_div,
    )

    return PortfolioGreeksSummary(
        asof=now_utc,
        positions=details,
        by_underlying=by_underlying,
        net_delta=net_delta,
        dollar_delta=dollar_delta,
        net_gamma=net_gamma,
        net_vega=net_vega,
        net_vega_per_1pct=net_vega_1pct,
        net_theta=net_theta,
        net_theta_per_day=net_theta_day,
        net_rho=net_rho,
        total_market_value=_money(total_mv),
        total_cost_basis=_money(total_cost),
        total_unrealized_pnl=_money(total_pnl),
        assumptions=assumptions,
        warnings=all_warnings,
    )
