"""Application service: imports, a cached analysis, and the queries built on it.

The analysis is recomputed only when the store's revision changes, so reads between
imports are served from memory and always describe one consistent dataset snapshot.
"""

from __future__ import annotations

import threading
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from tca.aggregate import DIFFICULTY_NOTE, VENUE_NOTE, summarize
from tca.attribution import Analysis, OrderTca, analyze
from tca.benchmark import ARRIVAL_MID, BENCHMARKS, Methodology
from tca.importer import (
    ParsedBatch,
    find_dataset_files,
    parse_csv,
    parse_file,
    parse_json,
)
from tca.metrics import Metrics
from tca.render import summary_row
from tca.report import methodology_view
from tca.store import ImportResult, Store
from tca.timeutil import format_ts


@dataclass(frozen=True)
class Filters:
    symbol: str | None = None
    broker: str | None = None
    strategy: str | None = None
    side: str | None = None
    venue: str | None = None
    start_ts: int | None = None  # arrival_ts >= start_ts
    end_ts: int | None = None  # arrival_ts < end_ts

    def matches(self, result: OrderTca) -> bool:
        order = result.order
        return (
            (self.symbol is None or order.symbol == self.symbol)
            and (self.broker is None or order.broker == self.broker)
            and (self.strategy is None or order.strategy == self.strategy)
            and (self.side is None or order.side.value == self.side)
            and (self.venue is None or self.venue in result.venues)
            and (self.start_ts is None or order.arrival_ts >= self.start_ts)
            and (self.end_ts is None or order.arrival_ts < self.end_ts)
        )

    def view(self) -> dict[str, Any]:
        return {
            "symbol": self.symbol,
            "broker": self.broker,
            "strategy": self.strategy,
            "side": self.side,
            "venue": self.venue,
            "start": format_ts(self.start_ts),
            "end": format_ts(self.end_ts),
        }


class TcaService:
    def __init__(
        self,
        store: Store | None = None,
        methodology: Methodology | None = None,
        metrics: Metrics | None = None,
    ):
        self.store = store or Store()
        self.methodology = methodology or Methodology()
        self.metrics = metrics or Metrics()
        self._lock = threading.Lock()
        self._cached: tuple[int, Analysis] | None = None

    # -- imports ---------------------------------------------------------------------

    def _commit(self, batch: ParsedBatch, source: str) -> ImportResult:
        result = self.store.insert(batch, source)
        rows = self.metrics.import_rows
        rows.labels(result.dataset, "accepted").inc(result.accepted)
        rows.labels(result.dataset, "duplicate").inc(result.duplicates)
        rows.labels(result.dataset, "rejected").inc(result.rejected)
        return result

    def import_csv(self, dataset: str, text: str, source: str = "") -> ImportResult:
        return self._commit(parse_csv(dataset, text), source)

    def import_json(self, dataset: str, body: str | bytes, source: str = "") -> ImportResult:
        return self._commit(parse_json(dataset, body), source)

    def import_file(self, dataset: str, path: Path) -> ImportResult:
        return self._commit(parse_file(dataset, path), path.name)

    def import_directory(self, directory: Path) -> list[ImportResult]:
        """Load every ``<dataset>.csv`` / ``.parquet`` found in a directory."""
        files = find_dataset_files(directory)
        if not files:
            raise FileNotFoundError(f"no dataset files (orders.csv, fills.csv, ...) in {directory}")
        return [self.import_file(dataset, path) for dataset, path in files.items()]

    # -- analysis --------------------------------------------------------------------

    def analysis(self) -> Analysis:
        with self._lock:
            if self._cached is not None and self._cached[0] == self.store.revision():
                return self._cached[1]
            started = time.perf_counter()
            revision, dataset = self.store.snapshot()
            analysis = analyze(dataset, self.methodology)
            self.metrics.analysis_seconds.observe(time.perf_counter() - started)
            self._publish(analysis)
            self._cached = (revision, analysis)
            return analysis

    def _publish(self, analysis: Analysis) -> None:
        filled = [result for result in analysis.results if result.fills]
        self.metrics.orders.labels("all").set(len(analysis.results))
        self.metrics.orders.labels("filled").set(len(filled))
        self.metrics.orphan_fills.set(analysis.orphan_fills)
        number = self.metrics.number
        for name in BENCHMARKS:
            trusted = sum(result.slippage[name].trusted for result in filled)
            self.metrics.trusted_share.labels(name).set(trusted / len(filled) if filled else 0)
            for totals in summarize(analysis.results, "all", name):
                self.metrics.shortfall_bps.labels(name).set(number(totals.slippage_bps))
        for totals in summarize(analysis.results, "all"):
            self.metrics.fill_rate.set(number(totals.fill_rate))
            first, last = totals.avg_time_to_first_fill_ms, totals.avg_time_to_last_fill_ms
            self.metrics.time_to_fill.labels("first").set(number(first and first / 1000))
            self.metrics.time_to_fill.labels("last").set(number(last and last / 1000))
        for group in summarize(analysis.results, "strategy"):
            self.metrics.cost_bps.labels(group.key, "fees").set(number(group.fee_bps))
            self.metrics.cost_bps.labels(group.key, "spread").set(number(group.spread_bps))

    def select(self, filters: Filters | None = None) -> tuple[Analysis, list[OrderTca]]:
        analysis = self.analysis()
        if filters is None:
            return analysis, list(analysis.results)
        return analysis, [result for result in analysis.results if filters.matches(result)]

    def summary(
        self,
        group_by: str = "all",
        benchmark: str = ARRIVAL_MID,
        filters: Filters | None = None,
        *,
        include_untrusted: bool = False,
        bucket_minutes: int = 30,
    ) -> dict[str, Any]:
        analysis, results = self.select(filters)
        options = {"include_untrusted": include_untrusted, "bucket_minutes": bucket_minutes}
        rows = summarize(results, group_by, benchmark, **options)
        totals = summarize(results, "all", benchmark, **options)
        notes = []
        if group_by in ("venue", "broker", "strategy"):
            notes.append(DIFFICULTY_NOTE)
        if group_by == "venue":
            notes.append(VENUE_NOTE)
        if benchmark != ARRIVAL_MID:
            notes.append("The spread/drift split is defined against arrival_mid only.")
        return {
            "group_by": group_by,
            "benchmark": benchmark,
            "include_untrusted": include_untrusted,
            "bucket_minutes": bucket_minutes if group_by == "time_bucket" else None,
            "filters": (filters or Filters()).view(),
            "rows": [summary_row(group, group_by, benchmark) for group in rows],
            "totals": summary_row(totals[0], "all", benchmark) if totals else None,
            "notes": notes,
            "methodology_version": analysis.methodology.id,
            "dataset_version": analysis.dataset.version,
        }

    def datasets(self) -> dict[str, Any]:
        analysis = self.analysis()
        dataset = analysis.dataset
        orders = dataset.orders
        return {
            "version": dataset.version,
            "versions": dataset.versions,
            "rows": dataset.counts,
            "orphan_fills": analysis.orphan_fills,
            "arrival_range": {
                "start": format_ts(min((o.arrival_ts for o in orders), default=None)),
                "end": format_ts(max((o.arrival_ts for o in orders), default=None)),
            },
            "dimensions": {
                "symbol": sorted({o.symbol for o in orders}),
                "broker": sorted({o.broker for o in orders}),
                "strategy": sorted({o.strategy for o in orders}),
                "venue": sorted({venue for result in analysis.results for venue in result.venues}),
            },
            "imports": self.store.imports(),
            "methodology": methodology_view(analysis.methodology),
        }
