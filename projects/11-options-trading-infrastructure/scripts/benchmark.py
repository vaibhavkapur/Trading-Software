"""Reproducible performance benchmark reporting machine details, p50/p99 latency, and memory."""

from __future__ import annotations

import platform
import statistics
import sys
import time
import tracemalloc
from decimal import Decimal

from options_infra.domain.models import OptionPosition, OptionRight
from options_infra.instruments.repository import InstrumentRepository
from options_infra.instruments.simulator import DEFAULT_ASOF
from options_infra.pricing.black_scholes import BSMInput, price_bsm
from options_infra.pricing.iv_solver import solve_implied_volatility
from options_infra.risk.aggregator import aggregate_portfolio_greeks
from options_infra.risk.scenarios import run_portfolio_scenarios


def _percentile(samples: list[float], pct: float) -> float:
    ordered = sorted(samples)
    idx = min(len(ordered) - 1, max(0, int(round((pct / 100.0) * (len(ordered) - 1)))))
    return ordered[idx]


def main() -> None:
    print("=== Options Trading Infrastructure — Reproducible Benchmark ===")
    print(
        f"Python: {sys.version.split()[0]} | "
        f"OS: {platform.platform()} | Arch: {platform.machine()}"
    )

    tracemalloc.start()

    # 1. Single-option BSM pricing (10,000 calls)
    bsm_n = 10_000
    bsm_inp = BSMInput(
        right=OptionRight.CALL,
        spot=100.0,
        strike=105.0,
        years=0.25,
        rate=0.04,
        dividend_yield=0.01,
        volatility=0.30,
    )
    bsm_lat_us: list[float] = []
    for _ in range(bsm_n):
        t0 = time.perf_counter_ns()
        price_bsm(bsm_inp)
        bsm_lat_us.append((time.perf_counter_ns() - t0) / 1_000.0)

    # 2. IV solver inversion (2,000 calls across strikes)
    iv_n = 2_000
    strikes = [85.0, 90.0, 95.0, 100.0, 105.0, 110.0, 115.0]
    iv_cases = [
        (
            k,
            price_bsm(
                BSMInput(
                    right=OptionRight.CALL,
                    spot=100.0,
                    strike=k,
                    years=0.25,
                    rate=0.04,
                    dividend_yield=0.0,
                    volatility=0.28,
                )
            ).price,
        )
        for k in strikes
    ]
    iv_lat_us: list[float] = []
    iv_errors = 0
    for i in range(iv_n):
        k, px = iv_cases[i % len(iv_cases)]
        t0 = time.perf_counter_ns()
        res = solve_implied_volatility(px, OptionRight.CALL, 100.0, k, 0.25, 0.04, 0.0)
        iv_lat_us.append((time.perf_counter_ns() - t0) / 1_000.0)
        if not res.converged:
            iv_errors += 1

    # 3. Portfolio Greeks + 21-cell 2D scenario grid (10-position portfolio, 200 iterations)
    repo = InstrumentRepository.from_default_simulator(asof=DEFAULT_ASOF)
    positions = [
        OptionPosition(contract_id="XYZ-20270115-90-P", quantity=-2, avg_price=Decimal("2.10")),
        OptionPosition(contract_id="XYZ-20270115-95-P", quantity=2, avg_price=Decimal("3.80")),
        OptionPosition(contract_id="XYZ-20270115-100-C", quantity=4, avg_price=Decimal("6.00")),
        OptionPosition(contract_id="XYZ-20270115-105-C", quantity=-4, avg_price=Decimal("3.60")),
        OptionPosition(contract_id="XYZ-20270115-110-C", quantity=2, avg_price=Decimal("2.10")),
        OptionPosition(contract_id="XYZ-20270319-95-C", quantity=2, avg_price=Decimal("9.50")),
        OptionPosition(contract_id="XYZ-20270319-105-C", quantity=-2, avg_price=Decimal("5.00")),
        OptionPosition(contract_id="XYZ-20270319-100-P", quantity=-1, avg_price=Decimal("6.20")),
        OptionPosition(contract_id="ABC-20270115-145-C", quantity=1, avg_price=Decimal("12.00")),
        OptionPosition(contract_id="ABC-20270115-155-C", quantity=-1, avg_price=Decimal("7.50")),
    ]
    risk_n = 200
    risk_lat_ms: list[float] = []
    for _ in range(risk_n):
        t0 = time.perf_counter()
        aggregate_portfolio_greeks(positions, repo, valuation_time=DEFAULT_ASOF)
        run_portfolio_scenarios(positions, repo, valuation_time=DEFAULT_ASOF)
        risk_lat_ms.append((time.perf_counter() - t0) * 1000.0)

    current_bytes, peak_bytes = tracemalloc.get_traced_memory()
    tracemalloc.stop()

    print(
        f"\n1. BSM Single Pricing ({bsm_n} runs): "
        f"p50={statistics.median(bsm_lat_us):.2f} us | "
        f"p99={_percentile(bsm_lat_us, 99):.2f} us | errors=0"
    )
    print(
        f"2. IV Solver ({iv_n} runs across 7 strikes): "
        f"p50={statistics.median(iv_lat_us):.2f} us | "
        f"p99={_percentile(iv_lat_us, 99):.2f} us | "
        f"convergence={(iv_n - iv_errors) / iv_n:.2%} | errors={iv_errors}"
    )
    print(
        f"3. 10-Position Portfolio Greeks + 21-Cell Scenario Grid ({risk_n} runs): "
        f"p50={statistics.median(risk_lat_ms):.3f} ms | "
        f"p99={_percentile(risk_lat_ms, 99):.3f} ms | errors=0"
    )
    print(
        f"4. Python Heap Memory (tracemalloc): "
        f"current={current_bytes / 1024:.1f} KiB | peak={peak_bytes / 1024:.1f} KiB"
    )


if __name__ == "__main__":
    main()
