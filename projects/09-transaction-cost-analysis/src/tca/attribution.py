"""Signed execution costs per parent order.

Sign convention, used everywhere: a cost is **positive when the execution was worse than
the benchmark**, for buys and sells alike. For a buy that means paying above the benchmark;
for a sell, receiving below it. ``Side.sign`` carries the flip.

Costs are computed in currency first and only then divided into basis points, always over
the benchmark notional (filled quantity x benchmark price). Because every bps figure shares
that denominator, components add up exactly: spread + drift = arrival shortfall, and
shortfall + fees = all-in cost.
"""

from __future__ import annotations

from collections import defaultdict
from dataclasses import dataclass, replace
from decimal import Decimal

from tca.benchmark import (
    ARRIVAL_MID,
    BENCHMARKS,
    DECISION_MID,
    INTERVAL_VWAP,
    MISSING,
    OK,
    BarBook,
    BenchmarkValue,
    Methodology,
    QuoteBook,
    bbo_flags,
    interval_vwap,
    quote_benchmark,
)
from tca.domain import ChildOrder, Fill, ParentOrder
from tca.store import Dataset
from tca.timeutil import US_PER_S

BPS = Decimal(10_000)
ZERO = Decimal(0)

# Problems with the order's own records. They make every benchmark comparison for the
# order untrustworthy, whatever the state of the market data.
INTEGRITY_FLAGS = frozenset({"FILL_BEFORE_ARRIVAL", "OVERFILLED"})


@dataclass(frozen=True)
class FillTca:
    fill: Fill
    bid: Decimal | None
    ask: Decimal | None
    quote_age_us: int | None
    quote_status: str
    spread_cost: Decimal | None  # sign * qty * (price - mid at fill)
    drift_cost: Decimal | None  # sign * qty * (mid at fill - arrival mid)

    @property
    def mid(self) -> Decimal | None:
        return None if self.bid is None else (self.bid + self.ask) / 2


@dataclass(frozen=True)
class Slippage:
    benchmark: str
    cost: Decimal | None  # currency; positive = worse than the benchmark
    notional: Decimal | None  # filled qty * benchmark price
    trusted: bool

    @property
    def bps(self) -> Decimal | None:
        return None if self.cost is None else self.cost / self.notional * BPS


@dataclass(frozen=True)
class OrderTca:
    order: ParentOrder
    child_orders: tuple[ChildOrder, ...]
    fills: tuple[FillTca, ...]
    filled_qty: int
    executed_notional: Decimal
    fees: Decimal
    benchmarks: dict[str, BenchmarkValue]
    slippage: dict[str, Slippage]
    spread_cost: Decimal | None  # both None unless every fill has a quote and arrival exists
    drift_cost: Decimal | None
    decomposition_trusted: bool
    flags: tuple[str, ...]
    notes: tuple[str, ...]

    @property
    def fill_rate(self) -> Decimal:
        return Decimal(self.filled_qty) / self.order.qty

    @property
    def avg_price(self) -> Decimal | None:
        return self.executed_notional / self.filled_qty if self.filled_qty else None

    @property
    def trusted(self) -> bool:
        """Whether the headline arrival-price shortfall can be relied on."""
        return self.slippage[ARRIVAL_MID].trusted

    @property
    def arrival_notional(self) -> Decimal | None:
        return self.slippage[ARRIVAL_MID].notional

    @property
    def fee_bps(self) -> Decimal | None:
        notional = self.arrival_notional
        return None if notional is None else self.fees / notional * BPS

    @property
    def total_cost(self) -> Decimal | None:
        cost = self.slippage[ARRIVAL_MID].cost
        return None if cost is None else cost + self.fees

    @property
    def effective_spread_bps(self) -> Decimal | None:
        """Twice the signed distance from the mid at each fill, quantity weighted, over the
        fills that have a prior quote."""
        quoted = [f for f in self.fills if f.spread_cost is not None]
        if not quoted:
            return None
        return (
            2
            * sum((f.spread_cost for f in quoted), ZERO)
            / sum((f.fill.qty * f.mid for f in quoted), ZERO)
            * BPS
        )

    @property
    def arrival_quoted_spread_bps(self) -> Decimal | None:
        arrival = self.benchmarks[ARRIVAL_MID]
        if arrival.value is None:
            return None
        return (arrival.details["ask"] - arrival.details["bid"]) / arrival.value * BPS

    @property
    def first_fill_ts(self) -> int | None:
        return self.fills[0].fill.ts if self.fills else None

    @property
    def last_fill_ts(self) -> int | None:
        return self.fills[-1].fill.ts if self.fills else None

    @property
    def time_to_first_fill_us(self) -> int | None:
        return None if not self.fills else self.first_fill_ts - self.order.arrival_ts

    @property
    def time_to_last_fill_us(self) -> int | None:
        return None if not self.fills else self.last_fill_ts - self.order.arrival_ts

    @property
    def participation_rate(self) -> Decimal | None:
        """Filled quantity over market volume in the bar-aligned VWAP window."""
        vwap = self.benchmarks[INTERVAL_VWAP]
        if vwap.value is None:
            return None
        return Decimal(self.filled_qty) / vwap.details["volume"]

    @property
    def venues(self) -> tuple[str, ...]:
        return tuple(
            sorted({f.fill.venue for f in self.fills} | {c.venue for c in self.child_orders})
        )


def analyze_order(
    order: ParentOrder,
    fills: list[Fill],
    child_orders: list[ChildOrder],
    quotes: QuoteBook,
    bars: BarBook,
    methodology: Methodology,
) -> OrderTca:
    fills = sorted(fills, key=lambda fill: (fill.ts, fill.fill_id))
    sign = order.side.sign
    filled_qty = sum(fill.qty for fill in fills)
    executed_notional = sum((fill.qty * fill.price for fill in fills), ZERO)
    fees = sum((fill.fee for fill in fills), ZERO)

    arrival = quote_benchmark(ARRIVAL_MID, "ARRIVAL", quotes, order.symbol, order.arrival_ts)
    decision = quote_benchmark(DECISION_MID, "DECISION", quotes, order.symbol, order.decision_ts)
    if fills:
        vwap = interval_vwap(bars, order.symbol, order.arrival_ts, fills[-1].ts)
    else:
        vwap = BenchmarkValue(INTERVAL_VWAP, None, MISSING, (), {})
    benchmarks = {ARRIVAL_MID: arrival, DECISION_MID: decision, INTERVAL_VWAP: vwap}

    flags: list[str] = []
    if not fills:
        flags.append("NO_FILLS")
    elif filled_qty < order.qty:
        flags.append("PARTIAL_FILL")
    elif filled_qty > order.qty:
        flags.append("OVERFILLED")
    if any(fill.ts < order.arrival_ts for fill in fills):
        flags.append("FILL_BEFORE_ARRIVAL")
    intact = not INTEGRITY_FLAGS.intersection(flags)

    fill_results: list[FillTca] = []
    fill_flags: set[str] = set()
    for fill in fills:
        # Strictly before the fill: a quote stamped at the same instant may be its result.
        bbo = quotes.bbo_at(order.symbol, fill.ts, inclusive=False)
        if bbo is None:
            fill_flags.add("FILL_QUOTE_MISSING")
            fill_results.append(FillTca(fill, None, None, None, MISSING, None, None))
            continue
        fill_flags.update(bbo_flags(bbo, "FILL"))
        spread = sign * fill.qty * (fill.price - bbo.mid)
        drift = None if arrival.value is None else sign * fill.qty * (bbo.mid - arrival.value)
        fill_results.append(FillTca(fill, bbo.bid, bbo.ask, bbo.age_us, bbo.status, spread, drift))

    slippage: dict[str, Slippage] = {}
    for name in BENCHMARKS:
        benchmark = benchmarks[name]
        if not fills or benchmark.value is None:
            slippage[name] = Slippage(name, None, None, False)
            continue
        notional = filled_qty * benchmark.value
        slippage[name] = Slippage(
            name, sign * (executed_notional - notional), notional, benchmark.trusted and intact
        )

    decomposed = bool(fills) and all(f.drift_cost is not None for f in fill_results)
    spread_cost = sum((f.spread_cost for f in fill_results), ZERO) if decomposed else None
    drift_cost = sum((f.drift_cost for f in fill_results), ZERO) if decomposed else None

    for benchmark in (arrival, decision, vwap):
        flags.extend(benchmark.flags)
    flags.extend(sorted(fill_flags))

    result = OrderTca(
        order=order,
        child_orders=tuple(sorted(child_orders, key=lambda c: (c.sent_ts, c.child_id))),
        fills=tuple(fill_results),
        filled_qty=filled_qty,
        executed_notional=executed_notional,
        fees=fees,
        benchmarks=benchmarks,
        slippage=slippage,
        spread_cost=spread_cost,
        drift_cost=drift_cost,
        decomposition_trusted=(
            decomposed
            and slippage[ARRIVAL_MID].trusted
            and all(f.quote_status == OK for f in fill_results)
        ),
        flags=tuple(flags),
        notes=(),
    )
    return _with_notes(result, methodology)


def _seconds(us: int) -> str:
    return f"{us / US_PER_S:.1f}s"


def _quote_notes(label: str, prefix: str, benchmark: BenchmarkValue, m: Methodology) -> list[str]:
    notes = []
    details = benchmark.details
    if f"{prefix}_QUOTE_MISSING" in benchmark.flags:
        notes.append(
            f"No quote at or before {label}, so the {label} benchmark is not computed. "
            "Later quotes are never used to backfill it."
        )
    if f"{prefix}_QUOTE_STALE" in benchmark.flags:
        notes.append(
            f"The newest quote at {label} was {_seconds(details['quote_age_us'])} old "
            f"(limit {m.max_quote_age_ms / 1000:g}s). The {label} benchmark is shown but "
            "excluded from trusted aggregates."
        )
    if f"{prefix}_QUOTE_CROSSED" in benchmark.flags:
        notes.append(
            f"The consolidated quote at {label} was crossed (bid {details['bid']} on "
            f"{details['bid_venue']} above ask {details['ask']} on {details['ask_venue']}); "
            "its mid is low confidence."
        )
    if f"{prefix}_CLOCK_SKEW" in benchmark.flags:
        notes.append(
            f"A quote used at {label} has event and receive times "
            f"{details['clock_skew_us'] / 1000:.0f}ms apart (limit {m.max_clock_skew_ms}ms), "
            "so its alignment with the order clock is uncertain."
        )
    return notes


def _with_notes(result: OrderTca, m: Methodology) -> OrderTca:
    order, flags, notes = result.order, set(result.flags), []
    if "NO_FILLS" in flags:
        notes.append(
            "No executions. Cost measures do not apply; the order counts toward fill rate."
        )
    if "PARTIAL_FILL" in flags:
        notes.append(
            f"Filled {result.filled_qty} of {order.qty}. Costs cover the executed quantity; "
            "the opportunity cost of the unfilled remainder is not estimated."
        )
    if "OVERFILLED" in flags:
        notes.append(
            f"Fills total {result.filled_qty} against an order quantity of {order.qty}. "
            "Check for duplicated or misattributed fills. Excluded from trusted aggregates."
        )
    if "FILL_BEFORE_ARRIVAL" in flags:
        notes.append(
            "At least one fill is timestamped before the order arrived, so the order and "
            "fill clocks disagree. Excluded from trusted aggregates."
        )
    notes += _quote_notes("arrival", "ARRIVAL", result.benchmarks[ARRIVAL_MID], m)
    notes += _quote_notes("decision", "DECISION", result.benchmarks[DECISION_MID], m)

    vwap = result.benchmarks[INTERVAL_VWAP]
    if "VWAP_WINDOW_INVALID" in flags:
        notes.append("The last fill precedes arrival, so there is no interval to take a VWAP over.")
    if "VWAP_BARS_MISSING" in flags:
        notes.append(
            "No bars with volume cover the order's interval. VWAP deviation is not computed."
        )
    if "VWAP_BARS_PARTIAL" in flags:
        details = vwap.details
        notes.append(
            f"{details['bars_expected'] - details['bars_found']} of {details['bars_expected']} "
            "bars in the order's interval are missing. Interval VWAP is low confidence."
        )
    if "VWAP_BARS_COARSE" in flags:
        details = vwap.details
        lifetime = details["order_end_ts"] - details["order_start_ts"]
        notes.append(
            f"The order ran for {_seconds(lifetime)}, less than one {details['bar_interval_s']}s "
            "bar, so the bar-aligned VWAP mostly reflects trading outside the order's life."
        )

    total = len(result.fills)
    missing = sum(1 for f in result.fills if f.quote_status == MISSING)
    degraded = sum(1 for f in result.fills if f.quote_status not in (OK, MISSING))
    if missing:
        notes.append(
            f"{missing} of {total} fills have no prior quote, so the spread/drift split is "
            "not computed for this order."
        )
    if degraded:
        notes.append(
            f"{degraded} of {total} fills were matched to a stale, crossed or clock-skewed "
            "quote. The spread/drift split is low confidence."
        )
    return replace(result, notes=tuple(notes))


@dataclass(frozen=True)
class Analysis:
    """Every order's result for one dataset snapshot under one methodology."""

    dataset: Dataset
    methodology: Methodology
    results: tuple[OrderTca, ...]  # ordered by arrival time, then order id
    by_id: dict[str, OrderTca]
    orphan_fills: int  # fills whose order_id matches no parent order
    quotes: QuoteBook
    bars: BarBook


def analyze(dataset: Dataset, methodology: Methodology) -> Analysis:
    quotes = QuoteBook(dataset.quotes, methodology)
    bars = BarBook(dataset.bars)
    fills_by_order: dict[str, list[Fill]] = defaultdict(list)
    for fill in dataset.fills:
        fills_by_order[fill.order_id].append(fill)
    children_by_order: dict[str, list[ChildOrder]] = defaultdict(list)
    for child in dataset.child_orders:
        children_by_order[child.order_id].append(child)

    known = {order.order_id for order in dataset.orders}
    orphan_fills = sum(
        len(fills) for order_id, fills in fills_by_order.items() if order_id not in known
    )

    results = tuple(
        analyze_order(
            order,
            fills_by_order.get(order.order_id, []),
            children_by_order.get(order.order_id, []),
            quotes,
            bars,
            methodology,
        )
        for order in sorted(dataset.orders, key=lambda o: (o.arrival_ts, o.order_id))
    )
    return Analysis(
        dataset=dataset,
        methodology=methodology,
        results=results,
        by_id={result.order.order_id: result for result in results},
        orphan_fills=orphan_fills,
        quotes=quotes,
        bars=bars,
    )
