"""Service-level telemetry and Prometheus metrics for the AI Quant Research Agent.

Exposes:
- Experiment reproducibility rate
- Rejected invalid/leaky experiments (by reason)
- Research cycle time
- Out-of-sample degradation versus in-sample
"""

from __future__ import annotations

import threading
from collections import Counter
from typing import Any

from prometheus_client import CollectorRegistry, Gauge, Histogram, generate_latest
from prometheus_client import Counter as PromCounter


class ResearchTelemetry:
    """Thread-safe metrics tracker with dedicated Prometheus CollectorRegistry."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self.registry = CollectorRegistry()

        self._rejected_counter = PromCounter(
            "quant_agent_rejected_experiments_total",
            "Total rejected invalid or leaky experiments by reason code",
            ["reason"],
            registry=self.registry,
        )
        self._runs_completed_counter = PromCounter(
            "quant_agent_runs_completed_total",
            "Total completed deterministic backtest runs",
            registry=self.registry,
        )
        self._repro_checks_total = PromCounter(
            "quant_agent_reproducibility_checks_total",
            "Total reproducibility replay checks",
            ["outcome"],
            registry=self.registry,
        )
        self._repro_rate_gauge = Gauge(
            "quant_agent_reproducibility_rate",
            "Experiment reproducibility rate in [0, 1]",
            registry=self.registry,
        )
        self._cycle_time_hist = Histogram(
            "quant_agent_research_cycle_seconds",
            "Experiment execution / research cycle duration in seconds",
            buckets=(0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 15.0, 60.0),
            registry=self.registry,
        )
        self._degradation_hist = Histogram(
            "quant_agent_is_vs_oos_sharpe_degradation",
            "In-sample minus out-of-sample Sharpe ratio degradation",
            buckets=(-2.0, -1.0, -0.5, 0.0, 0.25, 0.5, 1.0, 1.5, 2.0, 4.0),
            registry=self.registry,
        )
        self._latest_degradation_gauge = Gauge(
            "quant_agent_latest_is_vs_oos_degradation",
            "Most recent IS minus OOS Sharpe ratio degradation",
            registry=self.registry,
        )

        # In-memory summaries for JSON /v1/metrics endpoint
        self._rejected_by_reason: Counter[str] = Counter()
        self._repro_passed = 0
        self._repro_total = 0
        self._cycle_times_sec: list[float] = []
        self._degradations: list[float] = []
        self._runs_completed = 0

    def record_rejected_experiment(self, reason: str) -> None:
        with self._lock:
            self._rejected_by_reason[reason] += 1
            self._rejected_counter.labels(reason=reason).inc()

    def record_completed_run(
        self, *, duration_seconds: float, is_vs_oos_degradation: float
    ) -> None:
        with self._lock:
            self._runs_completed += 1
            self._cycle_times_sec.append(duration_seconds)
            self._degradations.append(is_vs_oos_degradation)
            self._runs_completed_counter.inc()
            self._cycle_time_hist.observe(duration_seconds)
            self._degradation_hist.observe(is_vs_oos_degradation)
            self._latest_degradation_gauge.set(is_vs_oos_degradation)

    def record_reproducibility_check(self, passed: bool) -> None:
        with self._lock:
            self._repro_total += 1
            if passed:
                self._repro_passed += 1
            rate = self._repro_passed / self._repro_total
            self._repro_checks_total.labels(outcome="pass" if passed else "fail").inc()
            self._repro_rate_gauge.set(rate)

    def snapshot(self) -> dict[str, Any]:
        with self._lock:
            repro_rate = (
                round(self._repro_passed / self._repro_total, 4) if self._repro_total > 0 else None
            )
            sorted_ct = sorted(self._cycle_times_sec)
            n_ct = len(sorted_ct)
            p50 = sorted_ct[(n_ct - 1) // 2] if n_ct else 0.0
            p99 = sorted_ct[min(n_ct - 1, round(0.99 * (n_ct - 1)))] if n_ct else 0.0
            mean_deg = (
                round(sum(self._degradations) / len(self._degradations), 4)
                if self._degradations
                else None
            )
            return {
                "runs_completed": self._runs_completed,
                "experiment_reproducibility_rate": repro_rate,
                "reproducibility_checks": {
                    "passed": self._repro_passed,
                    "total": self._repro_total,
                },
                "rejected_invalid_or_leaky_experiments": {
                    "total": sum(self._rejected_by_reason.values()),
                    "by_reason": dict(self._rejected_by_reason),
                },
                "research_cycle_time_seconds": {
                    "count": len(sorted_ct),
                    "p50": round(p50, 4),
                    "p99": round(p99, 4),
                },
                "out_of_sample_degradation_vs_in_sample": {
                    "count": len(self._degradations),
                    "mean_sharpe_drop": mean_deg,
                    "latest_sharpe_drop": (self._degradations[-1] if self._degradations else None),
                },
            }

    def prometheus_bytes(self) -> bytes:
        return generate_latest(self.registry)
