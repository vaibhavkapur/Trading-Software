"""Service metrics (Prometheus exposition format). One registry per app instance."""

from __future__ import annotations

from prometheus_client import CollectorRegistry, Counter, Histogram, generate_latest


class Metrics:
    def __init__(self) -> None:
        self.registry = CollectorRegistry()
        self.proposal_to_approval = Histogram(
            "atp_proposal_to_approval_seconds",
            "Time from proposal creation to the human decision",
            ["decision"],
            buckets=(1, 2, 5, 10, 30, 60, 120, 300, 900),
            registry=self.registry,
        )
        self.order_submissions = Counter(
            "atp_order_submissions_total",
            "Order submit requests by outcome",
            ["outcome"],
            registry=self.registry,
        )
        self.submit_retries = Counter(
            "atp_order_submit_retries_total",
            "Broker dispatch attempts that failed and were retried",
            registry=self.registry,
        )
        self.policy_rejections = Counter(
            "atp_policy_rejections_total",
            "Policy violations by reason code",
            ["reason", "stage"],
            registry=self.registry,
        )
        self.quote_staleness = Histogram(
            "atp_quote_staleness_seconds",
            "Age of the quote used for a risk decision",
            buckets=(0.1, 0.5, 1, 2, 5, 10, 30, 60),
            registry=self.registry,
        )
        self.reconciliation_mismatches = Counter(
            "atp_order_reconciliation_mismatch_total",
            "Times local order state disagreed with the broker and was repaired",
            ["kind"],
            registry=self.registry,
        )

    def render(self) -> bytes:
        return generate_latest(self.registry)

    def value(self, name: str, labels: dict[str, str] | None = None) -> float:
        """Read a sample; convenience for tests and the demo."""
        return self.registry.get_sample_value(name, labels or {}) or 0.0
