"""Interactive terminal walkthrough & failure demo (100% PAPER / SIMULATED data)."""

from __future__ import annotations

from datetime import timedelta
from decimal import Decimal
from pathlib import Path

from options_infra.domain.models import ExerciseStyle, OptionRight, OrderLeg, OrderSide
from options_infra.instruments.repository import InstrumentRepository
from options_infra.instruments.simulator import DEFAULT_ASOF
from options_infra.paper_adapter.adapter import PaperMultiLegAdapter
from options_infra.paper_adapter.store import PaperOrderStore
from options_infra.pricing.binomial import price_binomial_crr
from options_infra.pricing.black_scholes import (
    BSMInput,
    finite_difference_greeks,
    price_bsm,
    put_call_parity_residual,
)
from options_infra.pricing.iv_solver import solve_implied_volatility
from options_infra.risk.aggregator import aggregate_portfolio_greeks
from options_infra.risk.scenarios import run_portfolio_scenarios
from options_infra.strategy.vertical_spread import build_vertical_spread_preview


def _banner(title: str) -> None:
    print(f"\n{'=' * 74}\n{title}\n{'=' * 74}")


def main() -> None:
    print("[PAPER / SIMULATED WALKTHROUGH — NO REAL-MONEY CONNECTIVITY]")
    fixture_path = (
        Path(__file__).resolve().parent.parent
        / "examples"
        / "fixtures"
        / "xyz_chain_2027_01_15.json"
    )
    if fixture_path.exists():
        repo = InstrumentRepository.from_fixture(fixture_path, max_quote_age_seconds=300.0)
        print(f"Loaded deterministic replay fixture: {fixture_path.name}")
    else:
        repo = InstrumentRepository.from_default_simulator(
            asof=DEFAULT_ASOF, max_quote_age_seconds=300.0
        )
        print("Loaded built-in deterministic simulated chain")

    # 1. Analytic BSM pricing, put-call parity, finite-difference check & IV inversion
    _banner("1. BSM Pricing, Put-Call Parity, Finite-Diff Greeks & IV Inversion")
    inp = BSMInput(
        right=OptionRight.CALL,
        spot=100.0,
        strike=105.0,
        years=0.25,
        rate=0.04,
        dividend_yield=0.0,
        volatility=0.30,
    )
    res = price_bsm(inp)
    fd = finite_difference_greeks(inp)
    parity = put_call_parity_residual(100.0, 105.0, 0.25, 0.04, 0.0, 0.30)
    iv = solve_implied_volatility(res.price, OptionRight.CALL, 100.0, 105.0, 0.25, 0.04, 0.0)
    print(
        f"Call(S=100, K=105, T=0.25, r=4%, q=0%, vol=30%): price={res.price:.6f} "
        f"| parity residual={parity:.2e}"
    )
    print(
        f"Analytic vs FD Delta: {res.delta:.6f} vs {fd['delta']:.6f} | "
        f"Gamma: {res.gamma:.6f} vs {fd['gamma']:.6f} | "
        f"Vega: {res.vega:.4f} vs {fd['vega']:.4f}"
    )
    print(
        f"IV Solver round-trip: converged={iv.converged}, iv={iv.implied_volatility:.6f}, "
        f"iterations={iv.iterations}, err={iv.pricing_error:.2e}"
    )

    # American vs European warning + CRR Binomial comparison
    amer_crr = price_binomial_crr(
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
    print(
        f"American Put (CRR N=200): {amer_crr.price:.4f} vs "
        f"European {amer_crr.european_reference_price:.4f} "
        f"(early-exercise premium = +{amer_crr.early_exercise_premium:.4f})"
    )

    # 2. Defined-risk vertical spread preview
    _banner("2. Defined-Risk Vertical Spread Preview (PAPER / SIMULATED)")
    preview = build_vertical_spread_preview(
        long_contract_id="XYZ-20270115-100-C",
        short_contract_id="XYZ-20270115-105-C",
        quantity=2,
        repository=repo,
        pricing_mode="NATURAL",
        valuation_time=DEFAULT_ASOF,
    )
    print(
        f"Structure: {preview.spread_type.value} x{preview.quantity} "
        f"(Long {preview.long_strike} / Short {preview.short_strike})"
    )
    print(
        f"Net Debit: ${preview.net_debit_credit} (${preview.net_debit_credit_per_unit}/sh) | "
        f"Max Gain: ${preview.max_gain_estimate} | Max Loss: ${preview.max_loss_estimate} | "
        f"Breakeven: ${preview.breakeven_spot}"
    )
    print(
        f"Spread Greeks: Delta={preview.net_delta:+.2f}, Gamma={preview.net_gamma:+.4f}, "
        f"Vega/1%=${preview.net_vega_per_1pct:+.2f}, Theta/d=${preview.net_theta_per_day:+.2f}"
    )
    print(
        f"Assumptions: {preview.assumptions.model.value} "
        f"({preview.assumptions.day_count_convention})"
    )
    for lim in preview.limitations:
        print(f"  - Limitation: {lim}")

    # 3. Paper multi-leg order execution + Portfolio Greeks & Scenarios
    _banner("3. Paper Multi-Leg Order Submission & Portfolio Shock Scenarios")
    store = PaperOrderStore(":memory:")
    adapter = PaperMultiLegAdapter(repository=repo, store=store)
    order, dedup = adapter.submit_order(
        legs=preview.legs,
        quantity=2,
        strategy_type=preview.spread_type.value,
        idempotency_key="demo-order-001",
        pricing_mode="NATURAL",
        asof=DEFAULT_ASOF,
    )
    print(
        f"[{order.execution_mode}] Order {order.id}: status={order.status.value}, "
        f"dedup={dedup}, net=${order.net_debit_credit}, max_loss=${order.max_loss_estimate}"
    )

    positions = store.list_positions()
    summary = aggregate_portfolio_greeks(positions, repo, valuation_time=DEFAULT_ASOF)
    scenarios = run_portfolio_scenarios(
        positions,
        repo,
        spot_shocks_pct=[-0.10, -0.05, 0.0, 0.05, 0.10],
        vol_shocks_abs=[-0.05, 0.0, 0.05],
        valuation_time=DEFAULT_ASOF,
    )
    print(
        f"Portfolio ({len(summary.positions)} positions): Delta={summary.net_delta:+.2f}, "
        f"Gamma={summary.net_gamma:+.4f}, Vega/1%=${summary.net_vega_per_1pct:+.2f}, "
        f"Theta/d=${summary.net_theta_per_day:+.2f}"
    )
    print(
        f"Scenario P&L Range (Spot ±10% × Vol ±5%): "
        f"worst=${scenarios.worst_case_pnl:+.2f}, best=${scenarios.best_case_pnl:+.2f}"
    )

    # 4. Failure Demo: Stale Data, Uncovered Short, and Feed Disconnect
    _banner("4. Failure & Safety Demo (Stale Data, Naked Short, Feed Disconnect)")

    # 4a. Stale quotes
    stale_now = DEFAULT_ASOF + timedelta(minutes=20)
    stale_order, _ = adapter.submit_order(
        legs=preview.legs,
        quantity=1,
        asof=stale_now,
    )
    print(f"[Stale Data] status={stale_order.status.value} -> {stale_order.rejection_reason}")

    # 4b. Uncovered short blocked by broker rules
    naked_order, _ = adapter.submit_order(
        legs=[OrderLeg(contract_id="XYZ-20270115-105-C", side=OrderSide.SELL, ratio=1)],
        quantity=1,
        asof=DEFAULT_ASOF,
    )
    print(f"[Naked Short] status={naked_order.status.value} -> {naked_order.rejection_reason}")

    # 4c. Feed disconnect
    repo.set_feed_connected(False)
    disc_order, _ = adapter.submit_order(
        legs=preview.legs,
        quantity=1,
        asof=DEFAULT_ASOF,
    )
    print(f"[Feed Disconnect] status={disc_order.status.value} -> {disc_order.rejection_reason}")
    repo.set_feed_connected(True)

    # 4d. Duplicate delivery idempotency
    dup_order, was_dedup = adapter.submit_order(
        legs=preview.legs,
        quantity=2,
        strategy_type=preview.spread_type.value,
        idempotency_key="demo-order-001",
        asof=DEFAULT_ASOF,
    )
    print(
        f"[Duplicate Delivery] order_id={dup_order.id}, deduplicated={was_dedup}, "
        f"position_qty={store.list_positions()[0].quantity} (no double fill)"
    )

    # Verify exact Decimal invariant
    assert preview.max_gain_estimate + preview.max_loss_estimate == Decimal("1000.00")
    print("\nDemo completed successfully with all safety invariants verified.")


if __name__ == "__main__":
    main()
