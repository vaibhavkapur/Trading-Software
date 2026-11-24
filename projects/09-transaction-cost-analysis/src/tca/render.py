"""Turn results into plain rows for JSON and CSV.

Decimals are serialised as fixed-point strings, rounded half-even at the stated number of
places, so output is byte-stable and never passes through binary floating point.
"""

from __future__ import annotations

from decimal import ROUND_HALF_EVEN, Decimal
from typing import Any

from tca.aggregate import GroupSummary
from tca.attribution import BPS, Analysis, FillTca, OrderTca
from tca.benchmark import ARRIVAL_MID, BENCHMARKS, BenchmarkValue
from tca.domain import ChildOrder
from tca.timeutil import US_PER_MS, US_PER_S, format_ts

BPS_DP = 4
MONEY_DP = 4
PRICE_DP = 6
RATIO_DP = 6
MS_DP = 3

ROUNDING = {
    "bps_decimals": BPS_DP,
    "money_decimals": MONEY_DP,
    "price_decimals": PRICE_DP,
    "ratio_decimals": RATIO_DP,
    "mode": "half-even",
}


ORDER_COLUMNS = (
    "order_id",
    "symbol",
    "side",
    "broker",
    "strategy",
    "qty",
    "decision_ts",
    "arrival_ts",
    "filled_qty",
    "fill_rate",
    "fills",
    "avg_price",
    "executed_notional",
    "fees",
    *(
        f"{name}{suffix}"
        for name in BENCHMARKS
        for suffix in ("", "_status", "_cost", "_bps", "_trusted")
    ),
    "spread_cost",
    "spread_bps",
    "drift_cost",
    "drift_bps",
    "fee_bps",
    "total_cost",
    "total_bps",
    "effective_spread_bps",
    "arrival_quoted_spread_bps",
    "time_to_first_fill_ms",
    "time_to_last_fill_ms",
    "participation_rate",
    "trusted",
    "flags",
    "methodology_version",
    "dataset_version",
)
FILL_COLUMNS = (
    "fill_id",
    "order_id",
    "child_id",
    "venue",
    "side",
    "qty",
    "price",
    "fee",
    "ts",
    "bid",
    "ask",
    "mid",
    "quote_age_ms",
    "quote_status",
    "spread_cost",
    "drift_cost",
    "effective_spread_bps",
    "quoted_spread_bps",
)
BENCHMARK_COLUMNS = (
    "order_id",
    "name",
    "value",
    "status",
    "flags",
    "methodology_version",
    "details",
)
SUMMARY_COLUMNS = (
    "group_by",
    "key",
    "benchmark",
    "orders",
    "orders_filled",
    "orders_trusted",
    "trusted_share",
    "orders_costed",
    "fills",
    "order_qty",
    "routed_qty",
    "filled_qty",
    "fill_rate",
    "executed_notional",
    "fees",
    "benchmark_notional",
    "slippage_cost",
    "slippage_bps",
    "fee_bps",
    "total_cost",
    "total_bps",
    "orders_decomposed",
    "spread_cost",
    "spread_bps",
    "drift_cost",
    "drift_bps",
    "unsplit_cost",
    "unsplit_bps",
    "effective_spread_bps",
    "avg_quoted_spread_bps",
    "avg_time_to_first_fill_ms",
    "avg_time_to_last_fill_ms",
)


def num(value: Decimal | int | None, places: int) -> str | None:
    if value is None:
        return None
    rounded = Decimal(value).quantize(Decimal(1).scaleb(-places), rounding=ROUND_HALF_EVEN)
    return format(abs(rounded) if rounded == 0 else rounded, "f")


def ms(us: int | None) -> str | None:
    return None if us is None else num(Decimal(us) / US_PER_MS, MS_DP)


def _details(details: dict[str, Any]) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for key, value in details.items():
        if key.endswith("_ts"):
            out[key] = format_ts(value)
        elif key.endswith("_us"):
            out[f"{key[:-3]}_ms"] = ms(value)
        elif isinstance(value, Decimal):
            out[key] = num(value, PRICE_DP)
        else:
            out[key] = value
    return out


def benchmark_row(order_id: str, benchmark: BenchmarkValue, methodology_id: str) -> dict[str, Any]:
    return {
        "order_id": order_id,
        "name": benchmark.name,
        "value": num(benchmark.value, PRICE_DP),
        "status": benchmark.status,
        "flags": list(benchmark.flags),
        "methodology_version": methodology_id,
        "details": _details(benchmark.details),
    }


def order_row(result: OrderTca, analysis: Analysis) -> dict[str, Any]:
    """One flat row per parent order: the shape used by the order list and the CSV export."""
    order = result.order
    notional = result.arrival_notional
    row: dict[str, Any] = {
        "order_id": order.order_id,
        "symbol": order.symbol,
        "side": order.side.value,
        "broker": order.broker,
        "strategy": order.strategy,
        "qty": order.qty,
        "decision_ts": format_ts(order.decision_ts),
        "arrival_ts": format_ts(order.arrival_ts),
        "filled_qty": result.filled_qty,
        "fill_rate": num(result.fill_rate, RATIO_DP),
        "fills": len(result.fills),
        "avg_price": num(result.avg_price, PRICE_DP),
        "executed_notional": num(result.executed_notional, MONEY_DP),
        "fees": num(result.fees, MONEY_DP),
    }
    for name in BENCHMARKS:
        benchmark, slippage = result.benchmarks[name], result.slippage[name]
        row[name] = num(benchmark.value, PRICE_DP)
        row[f"{name}_status"] = benchmark.status
        row[f"{name}_cost"] = num(slippage.cost, MONEY_DP)
        row[f"{name}_bps"] = num(slippage.bps, BPS_DP)
        row[f"{name}_trusted"] = slippage.trusted
    row |= {
        "spread_cost": num(result.spread_cost, MONEY_DP),
        "spread_bps": num(_over(result.spread_cost, notional), BPS_DP),
        "drift_cost": num(result.drift_cost, MONEY_DP),
        "drift_bps": num(_over(result.drift_cost, notional), BPS_DP),
        "fee_bps": num(result.fee_bps, BPS_DP),
        "total_cost": num(result.total_cost, MONEY_DP),
        "total_bps": num(_over(result.total_cost, notional), BPS_DP),
        "effective_spread_bps": num(result.effective_spread_bps, BPS_DP),
        "arrival_quoted_spread_bps": num(result.arrival_quoted_spread_bps, BPS_DP),
        "time_to_first_fill_ms": ms(result.time_to_first_fill_us),
        "time_to_last_fill_ms": ms(result.time_to_last_fill_us),
        "participation_rate": num(result.participation_rate, RATIO_DP),
        "trusted": result.trusted,
        "flags": list(result.flags),
        "methodology_version": analysis.methodology.id,
        "dataset_version": analysis.dataset.version,
    }
    return row


def _over(cost: Decimal | None, notional: Decimal | None) -> Decimal | None:
    return None if cost is None or notional is None else cost / notional * BPS


def fill_row(f: FillTca, result: OrderTca) -> dict[str, Any]:
    fill, mid = f.fill, f.mid
    return {
        "fill_id": fill.fill_id,
        "order_id": fill.order_id,
        "child_id": fill.child_id,
        "venue": fill.venue,
        "side": result.order.side.value,
        "qty": fill.qty,
        "price": num(fill.price, PRICE_DP),
        "fee": num(fill.fee, MONEY_DP),
        "ts": format_ts(fill.ts),
        "bid": num(f.bid, PRICE_DP),
        "ask": num(f.ask, PRICE_DP),
        "mid": num(mid, PRICE_DP),
        "quote_age_ms": ms(f.quote_age_us),
        "quote_status": f.quote_status,
        "spread_cost": num(f.spread_cost, MONEY_DP),
        "drift_cost": num(f.drift_cost, MONEY_DP),
        "effective_spread_bps": num(
            None if mid is None else 2 * f.spread_cost / (fill.qty * mid) * BPS, BPS_DP
        ),
        "quoted_spread_bps": num(None if mid is None else (f.ask - f.bid) / mid * BPS, BPS_DP),
    }


def child_row(child: ChildOrder) -> dict[str, Any]:
    return {
        "child_id": child.child_id,
        "venue": child.venue,
        "qty": child.qty,
        "sent_ts": format_ts(child.sent_ts),
    }


def order_view(
    result: OrderTca, analysis: Analysis, benchmark: str = ARRIVAL_MID
) -> dict[str, Any]:
    """Full drill-down for one order. ``benchmark`` picks the headline comparison."""
    methodology_id = analysis.methodology.id
    row = order_row(result, analysis)
    chosen, slippage = result.benchmarks[benchmark], result.slippage[benchmark]
    return {
        "summary": row,
        "headline": {
            "benchmark": benchmark,
            "value": num(chosen.value, PRICE_DP),
            "status": chosen.status,
            "cost": num(slippage.cost, MONEY_DP),
            "bps": num(slippage.bps, BPS_DP),
            "trusted": slippage.trusted,
        },
        "benchmarks": [
            benchmark_row(result.order.order_id, result.benchmarks[name], methodology_id)
            for name in BENCHMARKS
        ],
        "fills": [fill_row(f, result) for f in result.fills],
        "child_orders": [child_row(child) for child in result.child_orders],
        "flags": list(result.flags),
        "notes": list(result.notes),
        "trusted": result.trusted,
        "methodology_version": methodology_id,
        "dataset_version": analysis.dataset.version,
    }


def overlay_view(result: OrderTca, analysis: Analysis, max_points: int = 1500) -> dict[str, Any]:
    """Consolidated quote path around the order, with fills and benchmark levels, for the
    dashboard's overlay chart."""
    order = result.order
    last = result.last_fill_ts if result.fills else order.arrival_ts
    first = min(order.decision_ts, order.arrival_ts, result.first_fill_ts or order.arrival_ts)
    last = max(last, order.arrival_ts)
    pad = max(15 * US_PER_S, (last - first) // 10)
    start, end = first - pad, last + pad

    times = [start, *analysis.quotes.update_times(order.symbol, start, end)]
    stride = -(-len(times) // max_points)
    quotes = []
    for ts in times[::stride]:
        bbo = analysis.quotes.bbo_at(order.symbol, ts)
        if bbo is not None:
            quotes.append(
                {
                    "ts": format_ts(ts),
                    "bid": num(bbo.bid, PRICE_DP),
                    "ask": num(bbo.ask, PRICE_DP),
                    "status": bbo.status,
                }
            )
    interval = analysis.bars.interval_us(order.symbol)
    bars = [
        {
            "ts": format_ts(slot),
            "end_ts": format_ts(slot + interval),
            "vwap": num(bar.vwap, PRICE_DP),
            "volume": bar.volume,
        }
        for slot, bar in analysis.bars.slots(order.symbol, start, end)
        if bar is not None
    ]
    vwap = result.benchmarks["interval_vwap"]
    return {
        "order_id": order.order_id,
        "symbol": order.symbol,
        "side": order.side.value,
        "start_ts": format_ts(start),
        "end_ts": format_ts(end),
        "decision_ts": format_ts(order.decision_ts),
        "arrival_ts": format_ts(order.arrival_ts),
        "last_fill_ts": format_ts(result.last_fill_ts),
        "quotes": quotes,
        "bars": bars,
        "fills": [
            {
                "ts": format_ts(f.fill.ts),
                "price": num(f.fill.price, PRICE_DP),
                "qty": f.fill.qty,
                "venue": f.fill.venue,
            }
            for f in result.fills
        ],
        "levels": {
            "arrival_mid": num(result.benchmarks["arrival_mid"].value, PRICE_DP),
            "decision_mid": num(result.benchmarks["decision_mid"].value, PRICE_DP),
            "interval_vwap": num(vwap.value, PRICE_DP),
            "avg_price": num(result.avg_price, PRICE_DP),
        },
        "vwap_window": {
            "start_ts": format_ts(vwap.details.get("window_start_ts")),
            "end_ts": format_ts(vwap.details.get("window_end_ts")),
        },
    }


def summary_row(group: GroupSummary, group_by: str, benchmark: str) -> dict[str, Any]:
    costed = bool(group.orders_costed)
    return {
        "group_by": group_by,
        "key": group.key,
        "benchmark": benchmark,
        "orders": group.orders,
        "orders_filled": group.orders_filled,
        "orders_trusted": group.orders_trusted,
        "trusted_share": num(group.trusted_share, RATIO_DP),
        "orders_costed": group.orders_costed,
        "fills": group.fills,
        "order_qty": group.order_qty if group.whole_orders else None,
        "routed_qty": None if group.whole_orders else group.routed_qty,
        "filled_qty": group.filled_qty,
        "fill_rate": num(group.fill_rate, RATIO_DP),
        "executed_notional": num(group.executed_notional, MONEY_DP),
        "fees": num(group.fees, MONEY_DP),
        "benchmark_notional": num(group.benchmark_notional if costed else None, MONEY_DP),
        "slippage_cost": num(group.slippage_cost if costed else None, MONEY_DP),
        "slippage_bps": num(group.slippage_bps, BPS_DP),
        "fee_bps": num(group.fee_bps, BPS_DP),
        "total_cost": num(group.total_cost, MONEY_DP),
        "total_bps": num(group.total_bps, BPS_DP),
        "orders_decomposed": group.orders_decomposed,
        "spread_cost": num(group.spread_cost if group.orders_decomposed else None, MONEY_DP),
        "spread_bps": num(group.spread_bps, BPS_DP),
        "drift_cost": num(group.drift_cost if group.orders_decomposed else None, MONEY_DP),
        "drift_bps": num(group.drift_bps, BPS_DP),
        "unsplit_cost": num(group.unsplit_cost, MONEY_DP),
        "unsplit_bps": num(group.unsplit_bps, BPS_DP),
        "effective_spread_bps": num(group.effective_spread_bps, BPS_DP),
        "avg_quoted_spread_bps": num(group.avg_quoted_spread_bps, BPS_DP),
        "avg_time_to_first_fill_ms": num(group.avg_time_to_first_fill_ms, MS_DP),
        "avg_time_to_last_fill_ms": num(group.avg_time_to_last_fill_ms, MS_DP),
    }
