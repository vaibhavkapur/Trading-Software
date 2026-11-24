"""Service-level Prometheus metrics. Each service instance owns its registry so tests and
multiple app instances never collide on global state.

The cost gauges mirror figures the API computes in ``Decimal``. They are converted to
floats here, at the monitoring boundary only; reports and API responses never use them.
"""

from __future__ import annotations

from decimal import Decimal

from prometheus_client import CollectorRegistry, Counter, Gauge, Histogram, generate_latest


class Metrics:
    def __init__(self) -> None:
        self.registry = CollectorRegistry()
        self.import_rows = Counter(
            "tca_import_rows_total",
            "Rows seen by the importer, by dataset and outcome.",
            ["dataset", "outcome"],
            registry=self.registry,
        )
        self.analysis_seconds = Histogram(
            "tca_analysis_seconds",
            "Time to recompute every order's result after the stored data changed.",
            registry=self.registry,
        )
        self.orders = Gauge(
            "tca_orders",
            "Parent orders in the latest analysis, by state.",
            ["state"],
            registry=self.registry,
        )
        self.trusted_share = Gauge(
            "tca_trusted_benchmark_share",
            "Share of filled orders whose benchmark is trusted.",
            ["benchmark"],
            registry=self.registry,
        )
        self.shortfall_bps = Gauge(
            "tca_shortfall_bps",
            "Slippage against the benchmark over all trusted orders, in basis points. "
            "Positive means worse than the benchmark.",
            ["benchmark"],
            registry=self.registry,
        )
        self.cost_bps = Gauge(
            "tca_cost_bps",
            "Fees and spread paid by strategy over trusted orders, in basis points of "
            "arrival notional.",
            ["strategy", "component"],
            registry=self.registry,
        )
        self.fill_rate = Gauge(
            "tca_fill_rate",
            "Filled quantity over ordered quantity, across all parent orders.",
            registry=self.registry,
        )
        self.time_to_fill = Gauge(
            "tca_time_to_fill_seconds",
            "Mean time from arrival to the first and to the last fill of an order.",
            ["fill"],
            registry=self.registry,
        )
        self.orphan_fills = Gauge(
            "tca_orphan_fills",
            "Fills whose order_id matches no parent order.",
            registry=self.registry,
        )

    @staticmethod
    def number(value: Decimal | None) -> float:
        """Gauge value for a figure that may be undefined (reported as NaN)."""
        return float("nan") if value is None else float(value)

    def render(self) -> bytes:
        return generate_latest(self.registry)
