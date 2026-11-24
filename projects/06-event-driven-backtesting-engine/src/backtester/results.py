"""The outcome of a run, and its canonical serialisations.

The four CSV renderings below are both the artifacts written to disk and the input to
``result_hash``. They contain only integers, fixed-point decimal strings and ISO
timestamps, so two runs agree on the hash exactly when they agree on every order, fill,
ledger entry and equity point.
"""

from __future__ import annotations

import csv
import hashlib
import io
from collections import Counter
from collections.abc import Iterable, Mapping
from dataclasses import dataclass, fields
from typing import Any

from .config import BacktestConfig
from .data import DataSummary
from .fixedpoint import fmt_units
from .metrics import Metrics, compute_metrics
from .orders import OrderStatus, OrderView, SimFill
from .portfolio import LedgerEntry, PortfolioSnapshot
from .timeutil import fmt_ts

_MONEY_FIELDS = frozenset(
    {
        "initial_equity",
        "final_equity",
        "pnl",
        "traded_notional",
        "fees",
        "shortfall",
        "min_cash",
    }
)
_TS_FIELDS = frozenset({"start_ts", "end_ts", "max_drawdown_peak_ts", "max_drawdown_trough_ts"})


@dataclass(frozen=True, slots=True)
class BacktestRun:
    """Identity of a run. Two runs with equal fields must produce equal results."""

    id: str
    config_hash: str
    data_version: str
    seed: int
    metrics: Metrics


@dataclass(frozen=True, slots=True)
class RuntimeStats:
    """Wall-clock measurements. Never part of any hash.

    ``seconds`` covers the event loop alone. ``total_seconds`` adds what follows it:
    reconciliation, metrics and hashing the results.
    """

    events: int
    seconds: float
    total_seconds: float = 0.0

    @property
    def events_per_second(self) -> float:
        return self.events / self.seconds if self.seconds > 0 else 0.0


@dataclass(frozen=True)
class BacktestResult:
    run: BacktestRun
    config: BacktestConfig
    strategy: Mapping[str, Any]
    data_summary: DataSummary
    orders: tuple[OrderView, ...]
    fills: tuple[SimFill, ...]
    ledger: tuple[LedgerEntry, ...]
    snapshots: tuple[PortfolioSnapshot, ...]
    metrics: Metrics
    in_sample: Metrics | None
    out_of_sample: Metrics | None
    reconciliation: Mapping[str, int]
    late_cancels: int
    result_hash: str
    runtime: RuntimeStats

    @property
    def order_status_counts(self) -> dict[str, int]:
        counts = Counter(order.status.value for order in self.orders)
        return {status.value: counts.get(status.value, 0) for status in OrderStatus}

    @property
    def reject_reasons(self) -> dict[str, int]:
        counts = Counter(
            order.reason or "UNKNOWN"
            for order in self.orders
            if order.status is OrderStatus.REJECTED
        )
        return dict(sorted(counts.items()))

    @property
    def cancel_reasons(self) -> dict[str, int]:
        counts = Counter(
            order.reason or "UNKNOWN"
            for order in self.orders
            if order.status is OrderStatus.CANCELED
        )
        return dict(sorted(counts.items()))

    @property
    def fill_ratio(self) -> float | None:
        """Filled quantity over the quantity of every order that reached the exchange."""
        sent = [o for o in self.orders if o.status is not OrderStatus.REJECTED]
        total = sum(o.qty for o in sent)
        return sum(o.filled_qty for o in sent) / total if total else None

    def segment_metrics(self, start: int | None = None, end: int | None = None) -> Metrics:
        """Metrics for [start, end), measured from the equity held just before ``start``."""
        return segment_metrics(self.snapshots, self.fills, self.orders, self.config, start, end)


def segment_metrics(
    snapshots: tuple[PortfolioSnapshot, ...],
    fills: Iterable[SimFill],
    orders: Iterable[OrderView],
    config: BacktestConfig,
    start: int | None = None,
    end: int | None = None,
) -> Metrics:
    base = config.initial_cash
    if start is not None:
        for snap in snapshots:
            if snap.ts >= start:
                break
            base = snap.equity
    return compute_metrics(
        snapshots,
        fills,
        {order.id: order.decision_px for order in orders},
        base_equity=base,
        periods_per_year=config.periods_per_year,
        risk_free_rate=float(config.risk_free_rate),
        start=start,
        end=end,
    )


# -- canonical serialisations -----------------------------------------------------------


def _csv(header: list[str], rows: Iterable[list[Any]]) -> str:
    buffer = io.StringIO()
    writer = csv.writer(buffer, lineterminator="\n")
    writer.writerow(header)
    writer.writerows(rows)
    return buffer.getvalue()


def _ts(value: int | None) -> str:
    return "" if value is None else fmt_ts(value)


def _px(value: int | None, decimals: int) -> str:
    if value is None:
        return ""
    if isinstance(value, bool) or not isinstance(value, int):
        return str(value)  # a rejected order can carry a malformed price; show it as given
    return fmt_units(value, decimals)


def orders_csv(orders: Iterable[OrderView], decimals: int) -> str:
    header = [
        "id",
        "symbol",
        "side",
        "type",
        "tif",
        "qty",
        "limit_px",
        "decision_px",
        "submitted_at",
        "arrived_at",
        "closed_at",
        "status",
        "filled_qty",
        "avg_fill_px",
        "fees",
        "reason",
        "tag",
    ]
    return _csv(
        header,
        (
            [
                o.id,
                o.symbol,
                o.side.value,
                o.type.value,
                o.tif.value,
                o.qty,
                _px(o.limit_px, decimals),
                _px(o.decision_px, decimals),
                _ts(o.submitted_at),
                _ts(o.arrived_at),
                _ts(o.closed_at),
                o.status.value,
                o.filled_qty,
                _px(o.avg_fill_px, decimals),
                fmt_units(o.fees, decimals),
                o.reason or "",
                o.tag or "",
            ]
            for o in orders
        ),
    )


def fills_csv(fills: Iterable[SimFill], decimals: int) -> str:
    header = ["id", "order_id", "ts", "symbol", "side", "qty", "px", "fee", "liquidity"]
    return _csv(
        header,
        (
            [
                f.id,
                f.order_id,
                fmt_ts(f.ts),
                f.symbol,
                f.side.value,
                f.qty,
                fmt_units(f.px, decimals),
                fmt_units(f.fee, decimals),
                f.liquidity.value,
            ]
            for f in fills
        ),
    )


def ledger_csv(ledger: Iterable[LedgerEntry], decimals: int) -> str:
    header = ["seq", "ts", "kind", "symbol", "qty", "px", "amount", "cash_after", "ref"]
    return _csv(
        header,
        (
            [
                e.seq,
                fmt_ts(e.ts),
                e.kind.value,
                e.symbol,
                e.qty,
                fmt_units(e.px, decimals),
                fmt_units(e.amount, decimals),
                fmt_units(e.cash_after, decimals),
                e.ref,
            ]
            for e in ledger
        ),
    )


def equity_csv(snapshots: Iterable[PortfolioSnapshot], decimals: int) -> str:
    header = ["ts", "cash", "equity", "holdings"]
    return _csv(
        header,
        (
            [
                fmt_ts(s.ts),
                fmt_units(s.cash, decimals),
                fmt_units(s.equity, decimals),
                ";".join(f"{sym}:{qty}@{fmt_units(px, decimals)}" for sym, qty, px in s.holdings),
            ]
            for s in snapshots
        ),
    )


def compute_result_hash(
    orders: Iterable[OrderView],
    fills: Iterable[SimFill],
    ledger: Iterable[LedgerEntry],
    snapshots: Iterable[PortfolioSnapshot],
    decimals: int,
) -> str:
    digest = hashlib.sha256()
    for name, text in (
        ("orders", orders_csv(orders, decimals)),
        ("fills", fills_csv(fills, decimals)),
        ("ledger", ledger_csv(ledger, decimals)),
        ("equity", equity_csv(snapshots, decimals)),
    ):
        digest.update(f"[{name}]\n".encode())
        digest.update(text.encode())
    return "sha256:" + digest.hexdigest()


def metrics_to_json(metrics: Metrics, decimals: int) -> dict[str, Any]:
    """Money as exact decimal strings, timestamps as ISO-8601, ratios rounded to 1e-10."""
    out: dict[str, Any] = {}
    for item in fields(metrics):
        value = getattr(metrics, item.name)
        if value is None:
            out[item.name] = None
        elif item.name in _MONEY_FIELDS:
            out[item.name] = fmt_units(value, decimals)
        elif item.name in _TS_FIELDS:
            out[item.name] = fmt_ts(value)
        elif isinstance(value, float):
            out[item.name] = round(value, 10)
        else:
            out[item.name] = value
    return out
