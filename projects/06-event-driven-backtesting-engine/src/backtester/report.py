"""Reports and reproducibility manifests.

Everything written by ``write_artifacts`` except ``runtime.json`` is a pure function of
the run's inputs, so re-running the same configuration over the same data reproduces the
directory byte for byte. Wall-clock measurements live only in ``runtime.json``.
"""

from __future__ import annotations

import hashlib
import json
import os
import platform
import shutil
import sys
import tempfile
from collections.abc import Mapping
from decimal import ROUND_HALF_EVEN, Decimal
from pathlib import Path
from typing import Any

from ._version import FILL_MODEL_VERSION, __version__
from .config import BacktestConfig
from .data import DataSummary
from .events import ActionType, CorporateAction, EventType
from .fixedpoint import fmt_units
from .metrics import Metrics, ReturnStats
from .results import (
    BacktestResult,
    equity_csv,
    fills_csv,
    ledger_csv,
    metrics_to_json,
    orders_csv,
)
from .timeutil import NS_PER_DAY, NS_PER_MS, NS_PER_SEC, fmt_date, fmt_ts
from .walkforward import WalkForwardResult

BANNER = (
    "SIMULATED RESULTS. Hypothetical trades against historical or synthetic data; no real "
    "orders were placed and no profitability is implied."
)
_MAX_GAPS_LISTED = 20


# -- formatting -------------------------------------------------------------------------


def money(units: int | None, decimals: int) -> str:
    """Display form: two decimal places with thousands separators. Not for storage."""
    if units is None:
        return "n/a"
    value = Decimal(fmt_units(units, decimals)).quantize(Decimal("0.01"), ROUND_HALF_EVEN)
    return f"{value:,.2f}"


def pct(value: float | None) -> str:
    return "n/a" if value is None else f"{value * 100:.2f}%"


def ratio(value: float | None, digits: int = 2) -> str:
    return "n/a" if value is None else f"{value:.{digits}f}"


def duration(ns: int | None) -> str:
    if ns is None:
        return "n/a"
    if ns == 0:
        return "0"
    for unit, size in (("d", NS_PER_DAY), ("h", 3600 * NS_PER_SEC), ("min", 60 * NS_PER_SEC)):
        if ns % size == 0:
            return f"{ns // size}{unit}"
    if ns % NS_PER_SEC == 0:
        return f"{ns // NS_PER_SEC}s"
    if ns % NS_PER_MS == 0:
        return f"{ns // NS_PER_MS}ms"
    return f"{ns}ns"


def _table(header: list[str], rows: list[list[str]]) -> list[str]:
    lines = ["| " + " | ".join(header) + " |", "|" + "|".join("---" for _ in header) + "|"]
    lines += ["| " + " | ".join(row) + " |" for row in rows]
    return lines


# -- disclosures ------------------------------------------------------------------------


def assumptions(config: BacktestConfig, summary: DataSummary) -> list[str]:
    """Every modelling assumption that affects the numbers, in plain words."""
    ex, risk = config.execution, config.risk
    kinds = {s.event_type for s in summary.symbols}
    out = []

    if ex.latency_ns:
        latency = f"{duration(ex.latency_ns)} after the strategy sends them"
    else:
        latency = "at the instant the strategy sends them"
    if ex.latency_jitter_ns:
        latency += f", plus up to {duration(ex.latency_jitter_ns)} of seeded random jitter"
    out.append(f"Latency: orders and cancels reach the exchange {latency}.")

    if EventType.BAR in kinds:
        touch = (
            "A bar that only touches the limit price does not fill it."
            if ex.touch_fill_probability == 0
            else f"A bar that only touches the limit price fills it with probability "
            f"{ex.touch_fill_probability} (seeded)."
        )
        out.append(
            "Bar fills: a bar can fill only orders that reached the exchange at or before "
            "its open. Market orders fill at that open. Limit orders fill at the open if "
            "marketable there, otherwise at their limit and only if the bar trades through "
            f"it. {touch} Fills are booked at the bar's close."
        )
        out.append(
            f"Spread: bars carry no quotes, so takers pay half of an assumed "
            f"{ex.spread_bps} bps bid/ask spread."
        )
    if EventType.QUOTE in kinds:
        age = (
            "of any age"
            if ex.max_quote_age_ns is None
            else f"no older than {duration(ex.max_quote_age_ns)}"
        )
        out.append(
            f"Quote fills: an arriving order trades against the prevailing quote ({age}); "
            "takers pay the quoted price, and a resting limit order fills at its limit once "
            "the other side of the book reaches it."
        )
    out.append(
        f"Slippage: {ex.slippage_bps} bps against the trader on every liquidity-taking fill, "
        "rounded up to a whole price unit. Passive limit fills are at the limit price."
    )
    if ex.participation_rate is None:
        out.append(
            "Liquidity: unlimited. Every eligible order fills in full regardless of traded "
            "volume, which overstates capacity for large orders."
        )
    else:
        out.append(
            f"Liquidity: fills are capped at {ex.participation_rate * 100:f}% of each bar's "
            "volume (or each quote's displayed size); the remainder keeps working."
        )
    fee = f"{ex.fee_bps} bps of notional"
    if ex.fee_per_unit:
        fee += f" plus {fmt_units(ex.fee_per_unit, config.price_decimals)} per unit"
    out.append(f"Fees: {fee}, rounded up, charged on every fill.")

    limits = [
        f"{name.replace('_', ' ')} {value}"
        for name, value in (
            ("max_order_qty", risk.max_order_qty),
            ("max_position_qty", risk.max_position_qty),
            ("max_open_orders", risk.max_open_orders),
        )
        if value is not None
    ]
    if risk.max_order_notional is not None:
        limits.append(f"max order notional {money(risk.max_order_notional, config.price_decimals)}")
    if risk.max_data_age_ns is not None:
        limits.append(f"orders refused when data is older than {duration(risk.max_data_age_ns)}")
    out.append(
        "Risk gate: short selling "
        + ("allowed" if risk.allow_short else "not allowed")
        + "; buying beyond available cash "
        + ("allowed" if risk.allow_leverage else "not allowed")
        + (". Limits: " + "; ".join(limits) if limits else "")
        + ". Cash is checked when an order is sent, at its reference price; a fill at a "
        "worse price can still overdraw the account (see minimum cash)."
    )
    out.append(
        "Corporate actions: splits adjust positions and cancel open orders in that symbol; "
        "fractional units are paid in cash. Dividends are credited in cash on the action "
        "date. No borrow cost, margin interest or tax is modelled."
    )
    out.append(
        "Accounting: average cost, exact integer arithmetic at "
        f"{config.price_decimals} decimal places. Positions are valued at the last published "
        "bar close or quote midpoint."
    )
    out.append(
        f"Return statistics: one observation per UTC day, annualised with "
        f"{config.periods_per_year} periods per year; risk-free rate {config.risk_free_rate}."
    )
    if config.trade_from is not None:
        out.append(
            f"Warm-up: the strategy observed data but could not trade before "
            f"{fmt_ts(config.trade_from)}; performance is measured from that time."
        )
    out.append("All timestamps are UTC.")
    return out


def split_statement(result: BacktestResult) -> str:
    config = result.config
    if config.oos_start is None:
        return (
            "None. This run has no out-of-sample split, so every figure below is in-sample "
            "and is not evidence that the strategy works. Use `walk-forward`, or pass "
            "`--oos-start`, to separate the data used for choosing parameters from the data "
            "used for evaluation."
        )
    return (
        f"In-sample is everything before {fmt_ts(config.oos_start)}; out-of-sample is "
        "everything from then on. The split is by time only: the out-of-sample figures are "
        "evidence only if nothing about the strategy or its parameters was chosen using "
        "data from that period, which the engine cannot check."
    )


def _describe_action(action: CorporateAction, decimals: int) -> str:
    if action.action_type is ActionType.SPLIT:
        return f"split {action.ratio_num}-for-{action.ratio_den}"
    return f"dividend {fmt_units(action.amount, decimals)} per unit"


def data_section(summary: DataSummary) -> list[str]:
    """Date ranges, per-symbol coverage, gaps and dropped rows."""
    lines = ["## Data", ""]
    if summary.first_ts is None:
        lines += ["The dataset is empty.", ""]
    else:
        lines += [
            f"{summary.event_count} market events and {len(summary.corporate_actions)} "
            f"corporate actions, from {fmt_ts(summary.first_ts)} to {fmt_ts(summary.last_ts)}.",
            "",
        ]
        lines += _table(
            ["Symbol", "Type", "Events", "First", "Last", "Interval", "Gaps"],
            [
                [
                    s.symbol,
                    s.event_type.value,
                    str(s.count),
                    fmt_ts(s.first_ts),
                    fmt_ts(s.last_ts),
                    duration(s.interval_ns),
                    str(len(s.gaps)),
                ]
                for s in summary.symbols
            ],
        )
        lines.append("")
    gaps = [gap for s in summary.symbols for gap in s.gaps]
    if gaps:
        lines.append(f"Data gaps ({len(gaps)}):")
        lines.append("")
        for gap in gaps[:_MAX_GAPS_LISTED]:
            lines.append(
                f"- {gap.symbol}: {gap.missing} missing {gap.unit} between "
                f"{fmt_ts(gap.after)} and {fmt_ts(gap.before)}"
            )
        if len(gaps) > _MAX_GAPS_LISTED:
            lines.append(f"- and {len(gaps) - _MAX_GAPS_LISTED} more")
        lines.append("")
    else:
        lines += ["No data gaps were detected.", ""]
    if summary.corporate_actions:
        lines += [f"Corporate actions ({len(summary.corporate_actions)}):", ""]
        lines += [
            f"- {fmt_ts(action.ts)} {action.symbol}: "
            f"{_describe_action(action, summary.price_decimals)}"
            for action in summary.corporate_actions
        ]
        lines.append("")
    lines += [
        f"Gap detection is a heuristic for a `{summary.calendar}` calendar and knows nothing "
        "about exchange holidays or session times. Orders do not fill during a gap, and "
        "positions stay valued at the last published price.",
        "",
        f"Exact duplicate rows dropped: {summary.duplicates_dropped}. "
        f"Invalid rows dropped: {summary.invalid_dropped}.",
        "",
    ]
    if summary.sources:
        lines += ["Source files:", ""]
        lines += [f"- `{name}` sha256 `{digest}`" for name, digest in summary.sources]
        lines.append("")
    return lines


# -- run report -------------------------------------------------------------------------


def render_report(result: BacktestResult) -> str:
    config = result.config
    decimals = config.price_decimals
    summary = result.data_summary
    lines = [f"# Backtest report: run {result.run.id}", "", f"> {BANNER}", ""]

    lines += ["## Reproducibility", ""]
    lines += _table(
        ["Field", "Value"],
        [
            ["Run id", f"`{result.run.id}`"],
            ["Strategy", f"`{result.strategy.get('name')}`"],
            ["Parameters", f"`{json.dumps(result.strategy.get('params', {}), sort_keys=True)}`"],
            ["Seed", str(config.seed)],
            ["Config hash", f"`{result.run.config_hash}`"],
            ["Data version", f"`{result.run.data_version}`"],
            ["Result hash", f"`{result.result_hash}`"],
            ["Engine / fill model", f"{__version__} / {FILL_MODEL_VERSION}"],
        ],
    )
    lines += [
        "",
        "The same configuration, data and seed reproduce the result hash exactly. "
        "`python -m backtester verify <this directory>` re-runs and checks it.",
        "",
    ]

    lines += data_section(summary)

    lines += ["## Assumptions", ""]
    lines += [f"- {text}" for text in assumptions(config, summary)]
    lines.append("")

    lines += ["## Out-of-sample split", "", split_statement(result), ""]

    lines += ["## Performance", ""]
    columns = [("Whole run", result.metrics)]
    if result.in_sample is not None and result.out_of_sample is not None:
        columns += [("In-sample", result.in_sample), ("Out-of-sample", result.out_of_sample)]
    lines += _table(["Metric"] + [name for name, _ in columns], _metric_rows(columns, decimals))
    lines.append("")

    lines += ["## Execution", ""]
    counts = result.order_status_counts
    lines += _table(
        ["Measure", "Value"],
        [
            ["Orders sent by the strategy", str(len(result.orders))],
            ["Filled in full", str(counts["filled"])],
            ["Rejected by the risk gate", _reasons(counts["rejected"], result.reject_reasons)],
            ["Canceled", _reasons(counts["canceled"], result.cancel_reasons)],
            [
                "Still open at the end",
                str(counts["pending"] + counts["working"] + counts["partially_filled"]),
            ],
            ["Fills", str(len(result.fills))],
            ["Fill ratio (quantity)", pct(result.fill_ratio)],
            ["Cancels that arrived too late", str(result.late_cancels)],
        ],
    )
    lines.append("")

    lines += ["## Reconciliation", ""]
    rec = result.reconciliation
    lines += [
        "Cash and positions were re-derived from the transaction ledger and from the fill "
        "list independently, and both agree with the portfolio. The accounting identity "
        "holds exactly:",
        "",
        "```text",
        f"initial cash      {money(config.initial_cash, decimals):>18}",
        f"+ realized P&L    {money(rec['realized_pnl'], decimals):>18}",
        f"+ unrealized P&L  {money(rec['unrealized_pnl'], decimals):>18}",
        f"- fees            {money(rec['fees'], decimals):>18}",
        f"+ dividends       {money(rec['dividends'], decimals):>18}",
        f"= final equity    {money(rec['equity'], decimals):>18}",
        "```",
        "",
        f"{rec['ledger_entries']} ledger entries, {rec['fills']} fills.",
        "",
    ]

    lines += ["## Positions at the end", ""]
    final = result.snapshots[-1] if result.snapshots else None
    if final is None or not final.holdings:
        lines += [f"Flat. Cash {money(final.cash if final else config.initial_cash, decimals)}."]
    else:
        lines += _table(
            ["Symbol", "Quantity", "Mark", "Value"],
            [
                [symbol, str(qty), fmt_units(px, decimals), money(qty * px, decimals)]
                for symbol, qty, px in final.holdings
            ],
        )
        lines += ["", f"Cash {money(final.cash, decimals)}."]
    lines.append("")
    return "\n".join(lines)


def _reasons(count: int, reasons: Mapping[str, int]) -> str:
    if not reasons:
        return str(count)
    return f"{count} ({', '.join(f'{name}: {n}' for name, n in reasons.items())})"


def _metric_rows(columns: list[tuple[str, Metrics]], decimals: int) -> list[list[str]]:
    def row(label: str, render) -> list[str]:
        return [label] + [render(metrics) for _, metrics in columns]

    def drawdown_window(m: Metrics) -> str:
        if m.max_drawdown_trough_ts is None:
            return "n/a"
        start = "start" if m.max_drawdown_peak_ts is None else fmt_date(m.max_drawdown_peak_ts)
        return f"{start} to {fmt_date(m.max_drawdown_trough_ts)}"

    return [
        row("Period", lambda m: _period(m)),
        row("Daily observations", lambda m: str(m.periods)),
        row("Starting equity", lambda m: money(m.initial_equity, decimals)),
        row("Final equity", lambda m: money(m.final_equity, decimals)),
        row("Net P&L", lambda m: money(m.pnl, decimals)),
        row("Total return", lambda m: pct(m.total_return)),
        row("Annualised return", lambda m: pct(m.annualized_return)),
        row("Annualised volatility", lambda m: pct(m.annualized_volatility)),
        row("Sharpe ratio", lambda m: ratio(m.sharpe)),
        row("Sortino ratio", lambda m: ratio(m.sortino)),
        row("Maximum drawdown", lambda m: pct(m.max_drawdown)),
        row("Drawdown peak to trough", drawdown_window),
        row("Traded notional", lambda m: money(m.traded_notional, decimals)),
        row("Turnover (x mean equity)", lambda m: ratio(m.turnover)),
        row("Fees", lambda m: money(m.fees, decimals)),
        row("Execution shortfall", lambda m: money(m.shortfall, decimals)),
        row("Execution shortfall (bps)", lambda m: ratio(m.shortfall_bps)),
        row("Fills", lambda m: str(m.fill_count)),
        row("Minimum cash", lambda m: money(m.min_cash, decimals)),
    ]


def _period(metrics: Metrics) -> str:
    if metrics.start_ts is None or metrics.end_ts is None:
        return "n/a"
    return f"{fmt_date(metrics.start_ts)} to {fmt_date(metrics.end_ts)}"


def summary_text(result: BacktestResult) -> str:
    """A short plain-text summary for the terminal."""
    m = result.metrics
    decimals = result.config.price_decimals
    counts = result.order_status_counts
    rows = [
        ("run id", result.run.id),
        ("strategy", f"{result.strategy.get('name')} {result.strategy.get('params', {})}"),
        ("period", _period(m)),
        ("final equity", f"{money(m.final_equity, decimals)}  ({pct(m.total_return)})"),
        ("sharpe / sortino", f"{ratio(m.sharpe)} / {ratio(m.sortino)}"),
        ("max drawdown", pct(m.max_drawdown)),
        ("turnover", f"{ratio(m.turnover)}x mean equity"),
        ("fees", money(m.fees, decimals)),
        ("shortfall", f"{money(m.shortfall, decimals)}  ({ratio(m.shortfall_bps)} bps)"),
        (
            "orders",
            f"{len(result.orders)} sent, {counts['filled']} filled, "
            f"{counts['rejected']} rejected, {counts['canceled']} canceled",
        ),
        ("data gaps", str(result.data_summary.gap_count)),
        ("reconciliation", "cash and positions match the ledger and fills"),
        ("config hash", result.run.config_hash),
        ("data version", result.run.data_version),
        ("result hash", result.result_hash),
        (
            "throughput",
            f"{result.runtime.events} events, event loop {result.runtime.seconds:.3f}s "
            f"({result.runtime.events_per_second:,.0f} events/s), "
            f"{result.runtime.total_seconds:.3f}s with reconciliation, metrics and hashing; "
            "this machine, this run",
        ),
    ]
    width = max(len(name) for name, _ in rows)
    return "\n".join(f"  {name:<{width}}  {value}" for name, value in rows)


# -- artifacts --------------------------------------------------------------------------


def _json(value: Any) -> str:
    return json.dumps(value, indent=2, sort_keys=True) + "\n"


def _data_summary_json(summary: DataSummary) -> dict[str, Any]:
    return {
        "first_ts": None if summary.first_ts is None else fmt_ts(summary.first_ts),
        "last_ts": None if summary.last_ts is None else fmt_ts(summary.last_ts),
        "event_count": summary.event_count,
        "corporate_actions": [
            {
                "ts": fmt_ts(action.ts),
                "symbol": action.symbol,
                "action": _describe_action(action, summary.price_decimals),
            }
            for action in summary.corporate_actions
        ],
        "duplicates_dropped": summary.duplicates_dropped,
        "invalid_dropped": summary.invalid_dropped,
        "calendar": summary.calendar,
        "sources": [{"file": name, "sha256": digest} for name, digest in summary.sources],
        "symbols": [
            {
                "symbol": s.symbol,
                "event_type": s.event_type.value,
                "count": s.count,
                "first_ts": fmt_ts(s.first_ts),
                "last_ts": fmt_ts(s.last_ts),
                "interval_ns": s.interval_ns,
                "gaps": [
                    {
                        "after": fmt_ts(g.after),
                        "before": fmt_ts(g.before),
                        "missing": g.missing,
                        "unit": g.unit,
                    }
                    for g in s.gaps
                ],
            }
            for s in summary.symbols
        ],
    }


def metrics_document(result: BacktestResult) -> dict[str, Any]:
    decimals = result.config.price_decimals
    return {
        "whole_run": metrics_to_json(result.metrics, decimals),
        "in_sample": (
            None if result.in_sample is None else metrics_to_json(result.in_sample, decimals)
        ),
        "out_of_sample": (
            None
            if result.out_of_sample is None
            else metrics_to_json(result.out_of_sample, decimals)
        ),
        "orders": result.order_status_counts,
        "reject_reasons": result.reject_reasons,
        "cancel_reasons": result.cancel_reasons,
        "fill_ratio": None if result.fill_ratio is None else round(result.fill_ratio, 10),
        "late_cancels": result.late_cancels,
        "reconciliation": {
            key: value if key in ("ledger_entries", "fills") else fmt_units(value, decimals)
            for key, value in result.reconciliation.items()
        },
    }


def deterministic_files(result: BacktestResult) -> dict[str, str]:
    """Every artifact that must be identical between two runs of the same inputs."""
    decimals = result.config.price_decimals
    return {
        "report.md": render_report(result),
        "metrics.json": _json(metrics_document(result)),
        "orders.csv": orders_csv(result.orders, decimals),
        "fills.csv": fills_csv(result.fills, decimals),
        "ledger.csv": ledger_csv(result.ledger, decimals),
        "equity.csv": equity_csv(result.snapshots, decimals),
    }


def build_manifest(
    result: BacktestResult, files: Mapping[str, str], inputs: Mapping[str, Any] | None
) -> dict[str, Any]:
    return {
        "run_id": result.run.id,
        "engine_version": __version__,
        "fill_model_version": FILL_MODEL_VERSION,
        "seed": result.run.seed,
        "config_hash": result.run.config_hash,
        "data_version": result.run.data_version,
        "result_hash": result.result_hash,
        "strategy": dict(result.strategy),
        "config": result.config.to_dict(),
        "inputs": dict(inputs) if inputs else None,
        "data": _data_summary_json(result.data_summary),
        "artifacts": {
            name: hashlib.sha256(text.encode()).hexdigest() for name, text in sorted(files.items())
        },
        "environment": {
            "python": platform.python_version(),
            "implementation": sys.implementation.name,
        },
        "simulated": True,
    }


def write_artifacts(
    result: BacktestResult, out_dir: str | Path, inputs: Mapping[str, Any] | None = None
) -> Path:
    """Write the run directory atomically: either all of it appears or none of it does."""
    files = deterministic_files(result)
    files["manifest.json"] = _json(build_manifest(result, files, inputs))
    runtime = {
        "note": "wall-clock measurements; not reproducible and not part of any hash",
        "events": result.runtime.events,
        "event_loop_seconds": round(result.runtime.seconds, 6),
        "event_loop_events_per_second": round(result.runtime.events_per_second, 1),
        "total_seconds": round(result.runtime.total_seconds, 6),
    }
    files["runtime.json"] = _json(runtime)
    return _write_directory(Path(out_dir), files)


def _write_directory(target: Path, files: Mapping[str, str]) -> Path:
    target.parent.mkdir(parents=True, exist_ok=True)
    staging = Path(tempfile.mkdtemp(prefix=f".{target.name}.", dir=target.parent))
    try:
        for name, text in files.items():
            (staging / name).write_text(text, encoding="utf-8")
        os.chmod(staging, 0o755)
        if target.exists():
            shutil.rmtree(target)
        os.rename(staging, target)
    except BaseException:
        shutil.rmtree(staging, ignore_errors=True)
        raise
    return target


# -- walk-forward report ----------------------------------------------------------------


def _stats_json(stats: ReturnStats) -> dict[str, Any]:
    return {
        "periods": stats.periods,
        "total_return": round(stats.total_return, 10),
        "annualized_return": _round(stats.annualized_return),
        "annualized_volatility": _round(stats.annualized_volatility),
        "sharpe": _round(stats.sharpe),
        "sortino": _round(stats.sortino),
        "max_drawdown": round(stats.max_drawdown, 10),
    }


def _round(value: float | None) -> float | None:
    return None if value is None else round(value, 10)


def walkforward_document(result: WalkForwardResult) -> dict[str, Any]:
    decimals = result.base_config.price_decimals
    wf = result.config
    return {
        "simulated": True,
        "data_version": result.data_version,
        "data": _data_summary_json(result.data_summary),
        "objective": wf.objective,
        "anchored": wf.anchored,
        "train_ns": wf.train_ns,
        "test_ns": wf.test_ns,
        "step_ns": wf.step_ns or wf.test_ns,
        "param_grid": {key: list(wf.param_grid[key]) for key in sorted(wf.param_grid)},
        "combinations": result.combinations,
        "backtests_run": result.backtests_run,
        "base_config": result.base_config.to_dict(),
        "out_of_sample": _stats_json(result.out_of_sample),
        "folds": [
            {
                "index": fold.index,
                "train": [fmt_ts(fold.train_start), fmt_ts(fold.train_end)],
                "test": [fmt_ts(fold.test_start), fmt_ts(fold.test_end)],
                "selected_params": dict(fold.selected.params),
                "in_sample": metrics_to_json(fold.selected.metrics, decimals),
                "out_of_sample": metrics_to_json(fold.test_metrics, decimals),
                "test_run_id": fold.test_result.run.id,
                "test_result_hash": fold.test_result.result_hash,
                "candidates": [
                    {"params": dict(c.params), "objective": _round(c.objective)}
                    for c in fold.candidates
                ],
            }
            for fold in result.folds
        ],
    }


def render_walkforward(result: WalkForwardResult) -> str:
    wf = result.config
    name = wf.objective.replace("_", " ")
    value = pct if wf.objective == "total_return" else ratio
    lines = ["# Walk-forward report", "", f"> {BANNER}", ""]
    lines += [
        "## Method",
        "",
        f"- {len(result.folds)} folds. Training window {duration(wf.train_ns)}"
        f"{' (anchored at the start of the data)' if wf.anchored else ' (rolling)'}, "
        f"test window {duration(wf.test_ns)}, step {duration(wf.step_ns or wf.test_ns)}.",
        f"- In each fold, {result.combinations} parameter combinations were run on the "
        f"training window and the one with the best {name} was run once on the test window.",
        "- A training run is given only data published before its test window begins. A "
        "test run sees the training window as warm-up, without trading, and is measured "
        "on the test window alone.",
        "- Each test window starts flat with the full initial cash; positions do not carry "
        "over, and the out-of-sample series chains the test windows' daily returns.",
        f"- {result.backtests_run} backtests were run in total. Choosing the best of "
        f"{result.combinations} combinations inflates in-sample results; no correction for "
        "multiple testing has been applied, so read the in-sample column as an upper bound.",
        f"- Data version `{result.data_version}`.",
        "",
    ]
    lines += data_section(result.data_summary)
    lines += ["## Folds", ""]
    rows = []
    for fold in result.folds:
        rows.append(
            [
                str(fold.index + 1),
                f"{fmt_date(fold.train_start)} to {fmt_date(fold.train_end)}",
                f"{fmt_date(fold.test_start)} to {fmt_date(fold.test_end)}",
                f"`{json.dumps(dict(fold.selected.params), sort_keys=True)}`",
                value(fold.selected.objective),
                value(fold.test_objective),
                pct(fold.test_metrics.total_return),
                pct(fold.test_metrics.max_drawdown),
                str(fold.test_metrics.fill_count),
            ]
        )
    lines += _table(
        [
            "Fold",
            "Train",
            "Test",
            "Selected parameters",
            f"In-sample {name}",
            f"Out-of-sample {name}",
            "OOS return",
            "OOS max drawdown",
            "OOS fills",
        ],
        rows,
    )
    oos = result.out_of_sample
    lines += [
        "",
        "Train and test ranges are half-open: each ends just before the date shown.",
        "",
        "## Out-of-sample result",
        "",
        "All test windows chained together. This is the only part of this report that is "
        "out-of-sample.",
        "",
    ]
    lines += _table(
        ["Metric", "Out-of-sample"],
        [
            ["Daily observations", str(oos.periods)],
            ["Total return", pct(oos.total_return)],
            ["Annualised return", pct(oos.annualized_return)],
            ["Annualised volatility", pct(oos.annualized_volatility)],
            ["Sharpe ratio", ratio(oos.sharpe)],
            ["Sortino ratio", ratio(oos.sortino)],
            ["Maximum drawdown (daily closes)", pct(oos.max_drawdown)],
        ],
    )
    lines += ["", "## Assumptions", ""]
    lines += [f"- {text}" for text in assumptions(result.base_config, result.data_summary)]
    lines.append("")
    return "\n".join(lines)


def write_walkforward(result: WalkForwardResult, out_dir: str | Path) -> Path:
    files = {
        "walkforward.md": render_walkforward(result),
        "walkforward.json": _json(walkforward_document(result)),
    }
    return _write_directory(Path(out_dir), files)
