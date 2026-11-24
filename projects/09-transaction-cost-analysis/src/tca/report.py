"""CSV exports and the versioned report bundle.

A bundle is a directory of CSVs plus ``manifest.json``. The manifest records the
methodology, every parameter, the content hash of each input dataset and the hash of each
output file. Nothing in a bundle depends on the wall clock, so the same inputs and
methodology always produce byte-identical files.
"""

from __future__ import annotations

import csv
import hashlib
import io
import json
from collections import Counter
from collections.abc import Iterable, Sequence
from pathlib import Path
from typing import Any

from tca import __version__
from tca.aggregate import DIFFICULTY_NOTE, GROUP_BYS, summarize
from tca.attribution import Analysis, OrderTca
from tca.benchmark import ARRIVAL_MID, BENCHMARKS, Methodology
from tca.render import (
    BENCHMARK_COLUMNS,
    FILL_COLUMNS,
    ORDER_COLUMNS,
    ROUNDING,
    SUMMARY_COLUMNS,
    benchmark_row,
    fill_row,
    order_row,
    summary_row,
)

SIGN_CONVENTION = (
    "Costs are positive when the execution was worse than the benchmark, for buys and "
    "sells alike: cost = side_sign * (execution price - benchmark price) * quantity, "
    "with side_sign +1 for buys and -1 for sells."
)

BENCHMARK_DEFINITIONS = {
    "arrival_mid": (
        "Mid of the consolidated best bid and offer in force at arrival_ts, using only "
        "quotes stamped at or before it."
    ),
    "decision_mid": "The same consolidated mid, taken at decision_ts.",
    "interval_vwap": (
        "Volume-weighted price of every trade bar touching [arrival_ts, last fill], "
        "using whole bars."
    ),
}


def to_csv(rows: Iterable[dict[str, Any]], columns: Sequence[str]) -> str:
    buffer = io.StringIO()
    writer = csv.writer(buffer, lineterminator="\n")
    writer.writerow(columns)
    for row in rows:
        writer.writerow([_cell(row.get(column)) for column in columns])
    return buffer.getvalue()


def _cell(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, list):
        return ";".join(str(item) for item in value)
    if isinstance(value, dict):
        return json.dumps(value, sort_keys=True, separators=(",", ":"))
    return str(value)


def orders_csv(results: Iterable[OrderTca], analysis: Analysis) -> str:
    return to_csv((order_row(result, analysis) for result in results), ORDER_COLUMNS)


def fills_csv(results: Iterable[OrderTca]) -> str:
    rows = (fill_row(f, result) for result in results for f in result.fills)
    return to_csv(rows, FILL_COLUMNS)


def benchmarks_csv(results: Iterable[OrderTca], analysis: Analysis) -> str:
    rows = (
        benchmark_row(result.order.order_id, result.benchmarks[name], analysis.methodology.id)
        for result in results
        for name in BENCHMARKS
    )
    return to_csv(rows, BENCHMARK_COLUMNS)


def summary_csv(
    results: Sequence[OrderTca],
    group_by: str,
    benchmark: str = ARRIVAL_MID,
    *,
    include_untrusted: bool = False,
    bucket_minutes: int = 30,
) -> str:
    groups = summarize(
        results,
        group_by,
        benchmark,
        include_untrusted=include_untrusted,
        bucket_minutes=bucket_minutes,
    )
    return to_csv((summary_row(group, group_by, benchmark) for group in groups), SUMMARY_COLUMNS)


def methodology_view(methodology: Methodology) -> dict[str, Any]:
    return {
        "id": methodology.id,
        "version": methodology.version,
        "parameters": methodology.params,
        "sign_convention": SIGN_CONVENTION,
        "bps_denominator": "filled quantity * benchmark price",
        "benchmarks": BENCHMARK_DEFINITIONS,
        "rounding": ROUNDING,
        "documentation": "docs/methodology.md",
    }


def _sha256(text: str) -> str:
    return f"sha256:{hashlib.sha256(text.encode()).hexdigest()}"


def build_bundle(
    analysis: Analysis,
    *,
    benchmark: str = ARRIVAL_MID,
    include_untrusted: bool = False,
    bucket_minutes: int = 30,
) -> dict[str, str]:
    """Return ``{file name: content}`` for a complete report, manifest included."""
    results = analysis.results
    files = {
        "orders.csv": orders_csv(results, analysis),
        "fills.csv": fills_csv(results),
        "benchmarks.csv": benchmarks_csv(results, analysis),
    }
    for group_by in GROUP_BYS:
        files[f"summary_by_{group_by}.csv"] = summary_csv(
            results,
            group_by,
            benchmark,
            include_untrusted=include_untrusted,
            bucket_minutes=bucket_minutes,
        )

    options = {
        "benchmark": benchmark,
        "include_untrusted": include_untrusted,
        "bucket_minutes": bucket_minutes,
    }
    filled = [result for result in results if result.fills]
    identity = json.dumps(
        [analysis.methodology.id, analysis.dataset.version, options, __version__], sort_keys=True
    )
    manifest = {
        "report_id": hashlib.sha256(identity.encode()).hexdigest()[:16],
        "tool": {"name": "tca", "version": __version__},
        "methodology": methodology_view(analysis.methodology),
        "dataset": {
            "version": analysis.dataset.version,
            "versions": analysis.dataset.versions,
            "rows": analysis.dataset.counts,
        },
        "options": options,
        "quality": {
            "orders": len(results),
            "orders_filled": len(filled),
            "orders_trusted": {
                name: sum(result.slippage[name].trusted for result in filled) for name in BENCHMARKS
            },
            "orphan_fills": analysis.orphan_fills,
            "flag_counts": dict(
                sorted(Counter(flag for result in results for flag in result.flags).items())
            ),
        },
        "notes": [DIFFICULTY_NOTE],
        "files": {
            name: {"sha256": _sha256(content), "rows": max(content.count("\n") - 1, 0)}
            for name, content in files.items()
        },
    }
    files["manifest.json"] = json.dumps(manifest, indent=2) + "\n"
    return files


def write_bundle(analysis: Analysis, out_dir: Path, **options: Any) -> dict[str, Any]:
    files = build_bundle(analysis, **options)
    out_dir.mkdir(parents=True, exist_ok=True)
    for name, content in files.items():
        (out_dir / name).write_text(content, encoding="utf-8", newline="")
    return json.loads(files["manifest.json"])
