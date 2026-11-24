"""Group-level execution quality.

Group figures are ratios of sums (total cost over total benchmark notional), never averages
of per-order basis points, so a 100-share order cannot outweigh a 100,000-share one.

Cost figures only include orders whose benchmark is trusted, unless the caller explicitly
asks for untrusted ones too. The row says how many orders were left out.
"""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass
from datetime import UTC, datetime
from decimal import Decimal

from tca.attribution import BPS, INTEGRITY_FLAGS, ZERO, FillTca, OrderTca
from tca.benchmark import ARRIVAL_MID, OK
from tca.timeutil import US_PER_MS, US_PER_S

GROUP_BYS = ("all", "broker", "symbol", "strategy", "side", "venue", "time_bucket")

DIFFICULTY_NOTE = (
    "Rows are not normalised for order difficulty (size, spread, volatility, urgency). "
    "A venue or broker that receives the harder orders will look worse here without "
    "executing worse, so do not rank rows on these figures alone."
)
VENUE_NOTE = (
    "Venue rows cover only the fills at that venue, measured against the parent order's "
    "benchmark. Fill rate is filled over routed child quantity. Time-to-fill is not "
    "attributed to venues."
)


def _ratio(numerator: Decimal, denominator: Decimal | int) -> Decimal | None:
    return numerator / denominator if denominator else None


def _bps(numerator: Decimal, denominator: Decimal) -> Decimal | None:
    return numerator / denominator * BPS if denominator else None


@dataclass
class GroupSummary:
    key: str
    whole_orders: bool  # False for venue rows, which hold only part of each order
    orders: int = 0
    orders_filled: int = 0
    orders_trusted: int = 0
    orders_costed: int = 0
    orders_decomposed: int = 0
    fills: int = 0
    order_qty: int = 0
    routed_qty: int = 0
    filled_qty: int = 0
    executed_notional: Decimal = ZERO
    fees: Decimal = ZERO
    benchmark_notional: Decimal = ZERO
    slippage_cost: Decimal = ZERO
    costed_fees: Decimal = ZERO
    spread_cost: Decimal = ZERO
    drift_cost: Decimal = ZERO
    _spread_paid: Decimal = ZERO
    _mid_notional: Decimal = ZERO
    _quoted_spread_bps_qty: Decimal = ZERO
    _quoted_qty: int = 0
    _first_fill_us: int = 0
    _last_fill_us: int = 0
    _timed_orders: int = 0

    @property
    def fill_rate(self) -> Decimal | None:
        denominator = self.order_qty if self.whole_orders else self.routed_qty
        return _ratio(Decimal(self.filled_qty), denominator)

    @property
    def trusted_share(self) -> Decimal | None:
        """Share of filled orders whose benchmark can be relied on."""
        return _ratio(Decimal(self.orders_trusted), self.orders_filled)

    @property
    def slippage_bps(self) -> Decimal | None:
        return _bps(self.slippage_cost, self.benchmark_notional)

    @property
    def fee_bps(self) -> Decimal | None:
        return _bps(self.costed_fees, self.benchmark_notional)

    @property
    def total_cost(self) -> Decimal | None:
        return self.slippage_cost + self.costed_fees if self.orders_costed else None

    @property
    def total_bps(self) -> Decimal | None:
        return _bps(self.slippage_cost + self.costed_fees, self.benchmark_notional)

    # The three parts below are contributions to this group's slippage and share its
    # denominator, so spread + drift + unsplit = slippage exactly.

    @property
    def spread_bps(self) -> Decimal | None:
        return _bps(self.spread_cost, self.benchmark_notional) if self.orders_decomposed else None

    @property
    def drift_bps(self) -> Decimal | None:
        return _bps(self.drift_cost, self.benchmark_notional) if self.orders_decomposed else None

    @property
    def unsplit_cost(self) -> Decimal | None:
        """Slippage of costed orders whose fills could not all be matched to a trusted
        quote, and so could not be divided into spread and drift."""
        if not self.orders_decomposed:
            return None
        return self.slippage_cost - self.spread_cost - self.drift_cost

    @property
    def unsplit_bps(self) -> Decimal | None:
        cost = self.unsplit_cost
        return None if cost is None else _bps(cost, self.benchmark_notional)

    @property
    def effective_spread_bps(self) -> Decimal | None:
        return _bps(2 * self._spread_paid, self._mid_notional)

    @property
    def avg_quoted_spread_bps(self) -> Decimal | None:
        """Quoted spread at the time of each fill, quantity weighted. Context for how wide
        the market was when this group traded, not a cost."""
        return _ratio(self._quoted_spread_bps_qty, self._quoted_qty)

    @property
    def avg_time_to_first_fill_ms(self) -> Decimal | None:
        return _ratio(Decimal(self._first_fill_us) / US_PER_MS, self._timed_orders)

    @property
    def avg_time_to_last_fill_ms(self) -> Decimal | None:
        return _ratio(Decimal(self._last_fill_us) / US_PER_MS, self._timed_orders)


def bucket_label(ts_us: int, bucket_minutes: int) -> str:
    bucket_us = bucket_minutes * 60 * US_PER_S
    start = ts_us - ts_us % bucket_us
    return datetime.fromtimestamp(start // US_PER_S, UTC).strftime("%Y-%m-%dT%H:%MZ")


def _slices(
    result: OrderTca, group_by: str, bucket_minutes: int
) -> list[tuple[str, tuple[FillTca, ...]]]:
    order = result.order
    if group_by == "venue":
        return [
            (venue, tuple(f for f in result.fills if f.fill.venue == venue))
            for venue in result.venues
        ]
    key = {
        "all": lambda: "all",
        "broker": lambda: order.broker,
        "symbol": lambda: order.symbol,
        "strategy": lambda: order.strategy,
        "side": lambda: order.side.value,
        "time_bucket": lambda: bucket_label(order.arrival_ts, bucket_minutes),
    }[group_by]()
    return [(key, result.fills)]


def summarize(
    results: Iterable[OrderTca],
    group_by: str = "all",
    benchmark: str = ARRIVAL_MID,
    *,
    include_untrusted: bool = False,
    bucket_minutes: int = 30,
) -> list[GroupSummary]:
    if group_by not in GROUP_BYS:
        raise ValueError(f"group_by must be one of {', '.join(GROUP_BYS)}")
    if bucket_minutes < 1:
        raise ValueError("bucket_minutes must be at least 1")
    whole = group_by != "venue"
    groups: dict[str, GroupSummary] = {}

    for result in results:
        order = result.order
        sign = order.side.sign
        slippage = result.slippage[benchmark]
        value = result.benchmarks[benchmark].value
        intact = not INTEGRITY_FLAGS.intersection(result.flags)

        for key, fills in _slices(result, group_by, bucket_minutes):
            group = groups.setdefault(key, GroupSummary(key, whole))
            qty = sum(f.fill.qty for f in fills)
            notional = sum((f.fill.qty * f.fill.price for f in fills), ZERO)
            fees = sum((f.fill.fee for f in fills), ZERO)

            group.orders += 1
            group.fills += len(fills)
            group.filled_qty += qty
            group.executed_notional += notional
            group.fees += fees
            if whole:
                group.order_qty += order.qty
            else:
                group.routed_qty += sum(c.qty for c in result.child_orders if c.venue == key)
            if not fills:
                continue

            group.orders_filled += 1
            group.orders_trusted += slippage.trusted
            costable = value is not None and (slippage.trusted or include_untrusted)
            if costable:
                group.orders_costed += 1
                group.benchmark_notional += qty * value
                group.slippage_cost += sign * (notional - qty * value)
                group.costed_fees += fees
            if (
                costable
                and benchmark == ARRIVAL_MID
                and result.spread_cost is not None
                and (result.decomposition_trusted or include_untrusted)
            ):
                group.orders_decomposed += 1
                group.spread_cost += sum((f.spread_cost for f in fills), ZERO)
                group.drift_cost += sum((f.drift_cost for f in fills), ZERO)

            for f in fills:
                usable = (f.quote_status == OK and intact) or include_untrusted
                if f.spread_cost is None or not usable:
                    continue
                group._spread_paid += f.spread_cost
                group._mid_notional += f.fill.qty * f.mid
                group._quoted_spread_bps_qty += f.fill.qty * (f.ask - f.bid) / f.mid * BPS
                group._quoted_qty += f.fill.qty

            if whole and (intact or include_untrusted):
                group._first_fill_us += result.time_to_first_fill_us
                group._last_fill_us += result.time_to_last_fill_us
                group._timed_orders += 1

    return [groups[key] for key in sorted(groups)]
