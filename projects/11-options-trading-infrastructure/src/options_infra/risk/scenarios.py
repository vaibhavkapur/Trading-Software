"""Portfolio shock scenario engine (spot shocks, volatility shocks, and time decay)."""

from __future__ import annotations

from datetime import UTC, datetime, timedelta

from pydantic import BaseModel, Field

from options_infra.domain.models import (
    ModelWarning,
    OptionPosition,
    PricingAssumptions,
    PricingModelType,
    WarningSeverity,
)
from options_infra.instruments.repository import InstrumentRepository
from options_infra.pricing.black_scholes import BSMInput, price_bsm, time_to_expiry_years
from options_infra.risk.aggregator import compute_contract_greeks


class ScenarioCell(BaseModel):
    spot_shock_pct: float
    vol_shock_abs: float
    days_forward: int
    scenario_pnl: float
    scenario_value: float
    net_delta: float
    net_gamma: float
    net_vega_per_1pct: float
    net_theta_per_day: float


class ScenarioGridReport(BaseModel):
    asof: datetime
    baseline_value: float
    spot_shocks_pct: list[float]
    vol_shocks_abs: list[float]
    days_forward: int
    cells: list[ScenarioCell]
    worst_case_pnl: float
    best_case_pnl: float
    assumptions: PricingAssumptions
    warnings: list[ModelWarning] = Field(default_factory=list)


def run_portfolio_scenarios(
    positions: list[OptionPosition],
    repository: InstrumentRepository,
    spot_shocks_pct: list[float] | None = None,
    vol_shocks_abs: list[float] | None = None,
    days_forward: int = 0,
    valuation_time: datetime | None = None,
    enforce_freshness: bool = False,
) -> ScenarioGridReport:
    """Revalue the portfolio across a 2D grid of spot shocks and parallel IV shocks."""
    if spot_shocks_pct is None:
        spot_shocks_pct = [-0.15, -0.10, -0.05, 0.0, 0.05, 0.10, 0.15]
    if vol_shocks_abs is None:
        vol_shocks_abs = [-0.05, 0.0, 0.05]
    if days_forward < 0:
        raise ValueError("days_forward must be >= 0")

    now_utc = (valuation_time or repository.latest_quote_time() or datetime.now(UTC)).astimezone(
        UTC
    )
    shock_time = now_utc + timedelta(days=days_forward)

    # Pre-resolve contracts, quotes, baseline values, and base IVs once
    resolved = []
    baseline_value = 0.0
    warnings: list[ModelWarning] = []
    seen_codes: set[str] = set()
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
        snap, used_vol, calc_warns = compute_contract_greeks(
            contract=contract,
            quote=quote,
            valuation_time=now_utc,
        )
        for w in repository.inspect_quote_health(quote, reference_now=now_utc) + calc_warns:
            if w.code not in seen_codes:
                seen_codes.add(w.code)
                warnings.append(w)

        scale = float(pos.quantity * contract.multiplier)
        base_px = snap.theoretical_price
        baseline_value += base_px * scale
        resolved.append((contract, quote, used_vol, scale))

    warnings.append(
        ModelWarning(
            code="SCENARIO_PARALLEL_SHOCK_ASSUMPTION",
            severity=WarningSeverity.INFO,
            message=(
                "Scenario grid applies parallel spot and volatility shocks with constant rates "
                "and European BSM revaluation; skew dynamics, liquidity widening, and early "
                "exercise are not modeled."
            ),
        )
    )

    cells: list[ScenarioCell] = []
    for v_shock in vol_shocks_abs:
        for s_shock in spot_shocks_pct:
            total_val = 0.0
            n_delta = 0.0
            n_gamma = 0.0
            n_vega_1pct = 0.0
            n_theta_day = 0.0

            for contract, quote, base_vol, scale in resolved:
                shocked_spot = max(0.01, float(quote.underlying_px) * (1.0 + s_shock))
                shocked_vol = max(0.01, base_vol + v_shock)
                rem_years = time_to_expiry_years(contract.expiry, shock_time)
                res = price_bsm(
                    BSMInput(
                        right=contract.right,
                        spot=shocked_spot,
                        strike=float(contract.strike),
                        years=rem_years,
                        rate=float(quote.rate),
                        dividend_yield=float(quote.dividend_yield),
                        volatility=shocked_vol,
                        style=contract.style,
                    )
                )
                total_val += res.price * scale
                n_delta += res.delta * scale
                n_gamma += res.gamma * scale
                n_vega_1pct += res.vega_per_1pct * scale
                n_theta_day += res.theta_per_day * scale

            cells.append(
                ScenarioCell(
                    spot_shock_pct=s_shock,
                    vol_shock_abs=v_shock,
                    days_forward=days_forward,
                    scenario_pnl=total_val - baseline_value,
                    scenario_value=total_val,
                    net_delta=n_delta,
                    net_gamma=n_gamma,
                    net_vega_per_1pct=n_vega_1pct,
                    net_theta_per_day=n_theta_day,
                )
            )

    pnls = [c.scenario_pnl for c in cells] or [0.0]
    assumptions = PricingAssumptions(
        model=PricingModelType.BLACK_SCHOLES_MERTON,
        continuous_rate=sample_rate,
        continuous_dividend_yield=sample_div,
    )
    return ScenarioGridReport(
        asof=now_utc,
        baseline_value=baseline_value,
        spot_shocks_pct=list(spot_shocks_pct),
        vol_shocks_abs=list(vol_shocks_abs),
        days_forward=days_forward,
        cells=cells,
        worst_case_pnl=min(pnls),
        best_case_pnl=max(pnls),
        assumptions=assumptions,
        warnings=warnings,
    )
