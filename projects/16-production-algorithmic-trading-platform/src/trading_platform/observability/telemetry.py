"""OpenTelemetry-compatible metrics, latency histograms, and structured span recorder."""

from __future__ import annotations

import math
import threading
import time
import uuid
from collections import defaultdict
from collections.abc import Callable, Iterator
from contextlib import contextmanager
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any

from trading_platform.domain.events import utc_now


@dataclass(frozen=True)
class TraceSpan:
    trace_id: str
    span_id: str
    name: str
    started_at_utc: datetime
    duration_ms: float
    status: str
    attributes: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "trace_id": self.trace_id,
            "span_id": self.span_id,
            "name": self.name,
            "started_at_utc": self.started_at_utc.isoformat(),
            "duration_ms": round(self.duration_ms, 4),
            "status": self.status,
            "attributes": self.attributes,
        }


class TelemetryRecorder:
    """In-process OTel-compatible metrics & tracing with optional external exporter hook."""

    def __init__(
        self,
        *,
        service_name: str = "production-algorithmic-trading-platform",
        span_exporter: Callable[[TraceSpan], None] | None = None,
        max_retained_spans: int = 5000,
    ) -> None:
        self.service_name = service_name
        self._span_exporter = span_exporter
        self._max_retained_spans = max_retained_spans
        self._lock = threading.RLock()
        self._counters: dict[str, int] = defaultdict(int)
        self._gauges: dict[str, float] = {}
        self._histograms: dict[str, list[float]] = defaultdict(list)
        self._spans: list[TraceSpan] = []

    def inc(self, name: str, value: int = 1) -> None:
        with self._lock:
            self._counters[name] += value

    def set_gauge(self, name: str, value: float) -> None:
        with self._lock:
            self._gauges[name] = float(value)

    def observe_ms(self, name: str, value_ms: float) -> None:
        with self._lock:
            self._histograms[name].append(float(value_ms))

    @contextmanager
    def span(
        self,
        name: str,
        *,
        attributes: dict[str, Any] | None = None,
        metric_name: str | None = None,
    ) -> Iterator[dict[str, Any]]:
        attrs: dict[str, Any] = dict(attributes or {})
        started_at = utc_now()
        t0 = time.perf_counter()
        status = "OK"
        try:
            yield attrs
        except Exception as exc:
            status = f"ERROR:{type(exc).__name__}"
            attrs["error.message"] = str(exc)
            raise
        finally:
            duration_ms = (time.perf_counter() - t0) * 1000.0
            span_obj = TraceSpan(
                trace_id=uuid.uuid4().hex,
                span_id=uuid.uuid4().hex[:16],
                name=name,
                started_at_utc=started_at,
                duration_ms=duration_ms,
                status=status,
                attributes=attrs,
            )
            with self._lock:
                self._spans.append(span_obj)
                if len(self._spans) > self._max_retained_spans:
                    self._spans = self._spans[-self._max_retained_spans :]
                if metric_name:
                    self._histograms[metric_name].append(duration_ms)
            if self._span_exporter is not None:
                self._span_exporter(span_obj)

    @staticmethod
    def _percentile(sorted_vals: list[float], pct: float) -> float:
        if not sorted_vals:
            return 0.0
        idx = min(
            len(sorted_vals) - 1,
            max(0, math.ceil((pct / 100.0) * len(sorted_vals)) - 1),
        )
        return sorted_vals[idx]

    def histogram_summary(self, name: str) -> dict[str, Any]:
        with self._lock:
            vals = sorted(self._histograms.get(name, []))
        if not vals:
            return {"count": 0, "min_ms": 0.0, "p50_ms": 0.0, "p95_ms": 0.0, "p99_ms": 0.0, "max_ms": 0.0}
        return {
            "count": len(vals),
            "min_ms": round(vals[0], 4),
            "mean_ms": round(sum(vals) / len(vals), 4),
            "p50_ms": round(self._percentile(vals, 50), 4),
            "p95_ms": round(self._percentile(vals, 95), 4),
            "p99_ms": round(self._percentile(vals, 99), 4),
            "max_ms": round(vals[-1], 4),
        }

    def snapshot(self) -> dict[str, Any]:
        with self._lock:
            hist_names = sorted(self._histograms.keys())
            counters = dict(sorted(self._counters.items()))
            gauges = dict(sorted(self._gauges.items()))
            recent_spans = [s.to_dict() for s in self._spans[-25:]]
        return {
            "service_name": self.service_name,
            "captured_at_utc": utc_now().isoformat(),
            "counters": counters,
            "gauges": gauges,
            "histograms": {name: self.histogram_summary(name) for name in hist_names},
            "recent_spans": recent_spans,
        }
