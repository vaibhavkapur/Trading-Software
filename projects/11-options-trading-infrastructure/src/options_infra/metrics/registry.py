"""Service-level metrics registry (Prometheus exposition + JSON summary).

Tracks all documented metrics:
- Pricing error against reference values
- IV solver convergence rate
- Risk recalculation latency
- Scenario P&L and Greek exposure
- Paper order outcomes
"""

from __future__ import annotations

from prometheus_client import CollectorRegistry, Counter, Gauge, Histogram, generate_latest


class OptionsMetricsRegistry:
    """Isolated per-app Prometheus registry (safe across tests and restarts)."""

    def __init__(self) -> None:
        self.registry = CollectorRegistry()
        self._iv_attempts = 0
        self._iv_converged = 0
        self._last_pricing_error = 0.0
        self._risk_latencies_ms: list[float] = []
        self._last_greeks: dict[str, float] = {
            "net_delta": 0.0,
            "net_gamma": 0.0,
            "net_vega_per_1pct": 0.0,
            "net_theta_per_day": 0.0,
        }
        self._last_scenario: dict[str, float] = {
            "worst_case_pnl": 0.0,
            "best_case_pnl": 0.0,
        }

        self.pricing_reference_error = Gauge(
            "options_pricing_reference_error_abs",
            "Absolute pricing error against analytical or put-call parity reference",
            registry=self.registry,
        )
        self.iv_attempts_total = Counter(
            "options_iv_solver_attempts_total",
            "Total implied volatility solver invocations",
            registry=self.registry,
        )
        self.iv_converged_total = Counter(
            "options_iv_solver_converged_total",
            "Total implied volatility solver invocations that converged",
            registry=self.registry,
        )
        self.iv_convergence_rate = Gauge(
            "options_iv_solver_convergence_rate",
            "Ratio of converged IV solver calls to total attempts",
            registry=self.registry,
        )
        self.risk_recalc_latency = Histogram(
            "options_risk_recalc_latency_seconds",
            "Portfolio Greek aggregation and scenario recalculation latency in seconds",
            buckets=(0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0),
            registry=self.registry,
        )
        self.portfolio_net_delta = Gauge(
            "options_portfolio_net_delta",
            "Latest computed portfolio net share-equivalent Delta",
            registry=self.registry,
        )
        self.portfolio_net_gamma = Gauge(
            "options_portfolio_net_gamma",
            "Latest computed portfolio net Gamma",
            registry=self.registry,
        )
        self.portfolio_net_vega_1pct = Gauge(
            "options_portfolio_net_vega_per_1pct",
            "Latest computed portfolio net Vega per 1 percentage point IV move",
            registry=self.registry,
        )
        self.portfolio_net_theta_day = Gauge(
            "options_portfolio_net_theta_per_day",
            "Latest computed portfolio net Theta per calendar day",
            registry=self.registry,
        )
        self.scenario_worst_pnl = Gauge(
            "options_scenario_worst_case_pnl",
            "Worst-case scenario P&L from latest shock grid",
            registry=self.registry,
        )
        self.scenario_best_pnl = Gauge(
            "options_scenario_best_case_pnl",
            "Best-case scenario P&L from latest shock grid",
            registry=self.registry,
        )
        self.paper_orders_total = Counter(
            "options_paper_orders_total",
            "Paper multi-leg orders processed by terminal/current status",
            labelnames=("status",),
            registry=self.registry,
        )

    def record_pricing_error(self, abs_error: float) -> None:
        self._last_pricing_error = abs(abs_error)
        self.pricing_reference_error.set(self._last_pricing_error)

    def record_iv_result(self, converged: bool) -> None:
        self._iv_attempts += 1
        self.iv_attempts_total.inc()
        if converged:
            self._iv_converged += 1
            self.iv_converged_total.inc()
        rate = self._iv_converged / self._iv_attempts if self._iv_attempts else 1.0
        self.iv_convergence_rate.set(rate)

    def record_risk_calculation(
        self,
        latency_seconds: float,
        net_delta: float,
        net_gamma: float,
        net_vega_per_1pct: float,
        net_theta_per_day: float,
        worst_case_pnl: float | None = None,
        best_case_pnl: float | None = None,
    ) -> None:
        self.risk_recalc_latency.observe(latency_seconds)
        self._risk_latencies_ms.append(latency_seconds * 1000.0)
        if len(self._risk_latencies_ms) > 500:
            self._risk_latencies_ms = self._risk_latencies_ms[-500:]

        self._last_greeks = {
            "net_delta": net_delta,
            "net_gamma": net_gamma,
            "net_vega_per_1pct": net_vega_per_1pct,
            "net_theta_per_day": net_theta_per_day,
        }
        self.portfolio_net_delta.set(net_delta)
        self.portfolio_net_gamma.set(net_gamma)
        self.portfolio_net_vega_1pct.set(net_vega_per_1pct)
        self.portfolio_net_theta_day.set(net_theta_per_day)

        if worst_case_pnl is not None:
            self._last_scenario["worst_case_pnl"] = worst_case_pnl
            self.scenario_worst_pnl.set(worst_case_pnl)
        if best_case_pnl is not None:
            self._last_scenario["best_case_pnl"] = best_case_pnl
            self.scenario_best_pnl.set(best_case_pnl)

    def record_paper_order(self, status: str) -> None:
        self.paper_orders_total.labels(status=status).inc()

    def render_prometheus(self) -> bytes:
        return generate_latest(self.registry)

    def snapshot_json(self) -> dict[str, object]:
        rate = self._iv_converged / self._iv_attempts if self._iv_attempts else 1.0
        avg_lat = (
            sum(self._risk_latencies_ms) / len(self._risk_latencies_ms)
            if self._risk_latencies_ms
            else 0.0
        )
        return {
            "pricing_reference_error_abs": self._last_pricing_error,
            "iv_solver_attempts": self._iv_attempts,
            "iv_solver_converged": self._iv_converged,
            "iv_solver_convergence_rate": round(rate, 6),
            "risk_recalc_samples": len(self._risk_latencies_ms),
            "risk_recalc_avg_latency_ms": round(avg_lat, 4),
            "latest_portfolio_greeks": self._last_greeks,
            "latest_scenario_pnl": self._last_scenario,
        }
