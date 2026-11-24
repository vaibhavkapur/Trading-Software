"""Deterministic fixture generator: a simulated market and simulated order flow.

Everything here is invented. Symbols, venues, brokers, prices, fees and fills come from a
seeded random walk, not from any real market, and nothing about the output says anything
about real execution quality.

The generator first simulates a consistent world, then damages the *captured* data in
known ways, so the analysis has realistic problems to detect:

* ``GLOBX`` quote capture starts five minutes late (missing arrival quotes),
* ``NOVA`` loses all quotes for three minutes (stale quotes during an outage),
* ``ACME`` is missing four one-minute bars (partial VWAP coverage),
* venue ``ALPHA`` runs its clock 1.8 s fast for five minutes (clock skew).
"""

from __future__ import annotations

import csv
import io
import random
from collections import defaultdict
from dataclasses import dataclass, field
from decimal import Decimal
from pathlib import Path

from tca.timeutil import US_PER_S, format_ts, parse_ts

DEFAULT_SEED = 20260302
SESSION_START = parse_ts("2026-03-02T14:30:00+00:00")
SESSION_SECONDS = 90 * 60
BAR_SECONDS = 60
TICK = Decimal("0.01")
RANDOM_ORDERS = 56


@dataclass(frozen=True)
class SymbolSpec:
    start_ticks: int
    half_spread: int  # ticks
    move_prob: float  # chance per second that the mid moves one tick
    max_volume: int  # other participants' volume per second is uniform in [0, max_volume)
    lots: tuple[int, int]  # parent order size range, in hundreds of shares
    impact_qty: int  # an aggressive child of this size moves the mid one tick 90% of the time


@dataclass(frozen=True)
class VenueSpec:
    extra_spread: int  # ticks added to the symbol's half spread
    taker_fee: Decimal  # per share
    maker_rebate: Decimal  # per share
    passive_fill_prob: float


@dataclass(frozen=True)
class StrategySpec:
    duration_s: tuple[int, int]
    children: tuple[int, int]
    aggressive_prob: float
    cleanup: bool  # send the unfilled remainder aggressively at the end


SYMBOLS = {
    "ACME": SymbolSpec(5_000, 1, 0.10, 600, (10, 80), 1_500),
    "GLOBX": SymbolSpec(12_000, 2, 0.30, 250, (5, 40), 800),
    "NOVA": SymbolSpec(1_850, 1, 0.05, 1_200, (20, 150), 4_000),
}
VENUES = {
    "ALPHA": VenueSpec(0, Decimal("0.0030"), Decimal("0.0020"), 0.55),
    "BRAVO": VenueSpec(0, Decimal("0.0025"), Decimal("0.0015"), 0.65),
    "CHARLIE": VenueSpec(1, Decimal("0.0010"), Decimal("0.0000"), 0.75),
}
BROKERS = {
    "NORTH": (5, 3, 2),  # routing weights for ALPHA, BRAVO, CHARLIE
    "SOUTH": (2, 3, 5),
}
STRATEGIES = {
    "LIQUIDITY_SEEK": StrategySpec((5, 40), (2, 4), 1.0, False),
    "TWAP": StrategySpec((180, 600), (6, 12), 0.5, True),
    "VWAP": StrategySpec((300, 720), (8, 14), 0.35, True),
    "PASSIVE": StrategySpec((240, 600), (4, 8), 0.0, False),
}
QUOTE_PROB = 0.3  # chance per second that a venue refreshes its quote in a symbol


def _at(clock: str) -> int:
    return parse_ts(f"2026-03-02T{clock}+00:00")


# Damage applied to the captured data after the simulation.
QUOTE_GAPS = (
    ("GLOBX", SESSION_START, _at("14:35:00")),
    ("NOVA", _at("15:10:00"), _at("15:13:00")),
)
BAR_GAPS = (("ACME", _at("15:20:00"), _at("15:24:00")),)
SKEW_VENUE, SKEW_START, SKEW_END, SKEW_US = "ALPHA", _at("15:30:00"), _at("15:35:00"), 1_800_000

# Orders placed deliberately so that each kind of damage affects at least one order:
# (clock, symbol, side, hundreds of shares, broker, strategy)
PLANTED_ORDERS = (
    ("14:33:10", "GLOBX", "buy", 6, "NORTH", "LIQUIDITY_SEEK"),
    ("15:10:40", "NOVA", "sell", 60, "SOUTH", "TWAP"),
    ("15:11:30", "NOVA", "buy", 30, "NORTH", "LIQUIDITY_SEEK"),
    ("15:18:30", "ACME", "buy", 40, "SOUTH", "TWAP"),
    ("15:31:15", "ACME", "sell", 20, "NORTH", "LIQUIDITY_SEEK"),
)


@dataclass
class SimData:
    orders: list[dict] = field(default_factory=list)
    child_orders: list[dict] = field(default_factory=list)
    fills: list[dict] = field(default_factory=list)
    quotes: list[dict] = field(default_factory=list)
    bars: list[dict] = field(default_factory=list)


COLUMNS = {
    "orders": (
        "order_id",
        "symbol",
        "side",
        "qty",
        "decision_ts",
        "arrival_ts",
        "broker",
        "strategy",
    ),
    "child_orders": ("child_id", "order_id", "venue", "qty", "sent_ts"),
    "fills": ("fill_id", "order_id", "child_id", "venue", "qty", "price", "fee", "ts"),
    "quotes": ("symbol", "venue", "bid", "ask", "ts", "recv_ts"),
    "bars": ("symbol", "ts", "interval_s", "vwap", "volume"),
}


@dataclass
class _Parent:
    order_id: str
    symbol: str
    sign: int
    qty: int
    broker: str
    strategy: str
    children: int = 0
    filled: int = 0


def _price(ticks: int) -> str:
    return format(ticks * TICK, ".2f")


def _venue_clock(venue: str, ts: int) -> int:
    """What the venue stamps on an event that truly happened at ``ts``."""
    return ts + SKEW_US if venue == SKEW_VENUE and SKEW_START <= ts < SKEW_END else ts


def simulate(seed: int = DEFAULT_SEED) -> SimData:
    rng = random.Random(seed)
    data = SimData()
    venue_names = list(VENUES)
    mids = {symbol: spec.start_ticks for symbol, spec in SYMBOLS.items()}
    venue_quotes: dict[tuple[str, str], tuple[int, int]] = {}
    bar_notional: dict[tuple[str, int], int] = defaultdict(int)
    bar_volume: dict[tuple[str, int], int] = defaultdict(int)
    # (second, symbol) -> [(offset_us, kind, payload)], consumed in time order
    schedule: dict[tuple[int, str], list[tuple[int, str, tuple]]] = defaultdict(list)
    passive_children: set[str] = set()
    fill_counts: dict[str, int] = defaultdict(int)

    def trade(symbol: str, ts: int, price_ticks: int, qty: int) -> None:
        minute = ts - ts % (BAR_SECONDS * US_PER_S)
        bar_notional[symbol, minute] += price_ticks * qty
        bar_volume[symbol, minute] += qty

    def emit_quote(symbol: str, venue: str, ts: int) -> tuple[int, int]:
        half = SYMBOLS[symbol].half_spread + VENUES[venue].extra_spread
        bid = mids[symbol] - half - (rng.random() < 0.2)
        ask = mids[symbol] + half + (rng.random() < 0.2)
        venue_quotes[symbol, venue] = (bid, ask)
        data.quotes.append(
            {
                "symbol": symbol,
                "venue": venue,
                "bid": _price(bid),
                "ask": _price(ask),
                "true_ts": ts,
                "ts": _venue_clock(venue, ts),
                "recv_ts": ts + rng.randrange(300, 2_500),
            }
        )
        return bid, ask

    def record_fill(parent: _Parent, child_id: str, venue: str, qty: int, ticks: int, ts: int):
        spec = VENUES[venue]
        if child_id in passive_children:
            fee = -(qty * spec.maker_rebate) if spec.maker_rebate else Decimal(0)
        else:
            fee = qty * spec.taker_fee
        fill_number = fill_counts[child_id] = fill_counts[child_id] + 1
        data.fills.append(
            {
                "fill_id": f"{child_id}-F{fill_number}",
                "order_id": parent.order_id,
                "child_id": child_id,
                "venue": venue,
                "qty": qty,
                "price": _price(ticks),
                "fee": format(fee, ".4f"),
                "ts": _venue_clock(venue, ts),
            }
        )
        parent.filled += qty
        trade(parent.symbol, ts, ticks, qty)

    def new_child(parent: _Parent, venue: str, qty: int, ts: int) -> str:
        parent.children += 1
        child_id = f"{parent.order_id}-C{parent.children:02d}"
        data.child_orders.append(
            {
                "child_id": child_id,
                "order_id": parent.order_id,
                "venue": venue,
                "qty": qty,
                "sent_ts": ts,
            }
        )
        return child_id

    def send_aggressive(parent: _Parent, venue: str, qty: int, ts: int) -> None:
        child_id = new_child(parent, venue, qty, ts)
        bid, ask = emit_quote(parent.symbol, venue, ts - 150)
        touch = ask if parent.sign > 0 else bid
        fill_ts = ts + rng.randrange(100, 900)
        if qty >= 400 and rng.random() < 0.5:
            # Larger children take the touch and then the next level, one tick worse.
            first = (qty * 6 // 10) // 100 * 100
            record_fill(parent, child_id, venue, first, touch, fill_ts)
            fill_ts += rng.randrange(200, 2_000)
            record_fill(parent, child_id, venue, qty - first, touch + parent.sign, fill_ts)
        else:
            record_fill(parent, child_id, venue, qty, touch, fill_ts)
        if rng.random() < min(0.9, qty / SYMBOLS[parent.symbol].impact_qty * 0.9):
            mids[parent.symbol] += parent.sign

    def send_passive(parent: _Parent, venue: str, qty: int, ts: int, second: int) -> None:
        child_id = new_child(parent, venue, qty, ts)
        passive_children.add(child_id)
        bid, ask = emit_quote(parent.symbol, venue, ts - 150)
        posted = bid if parent.sign > 0 else ask
        check_second = second + 1 + rng.randrange(8)
        schedule[check_second, parent.symbol].append(
            (rng.randrange(US_PER_S), "passive_check", (parent, child_id, venue, qty, posted))
        )

    def check_passive(payload: tuple, ts: int) -> None:
        parent, child_id, venue, qty, posted = payload
        half = SYMBOLS[parent.symbol].half_spread
        # Where the near touch is now, on the side the order rests on.
        touch = mids[parent.symbol] - parent.sign * half
        behind = (touch - posted) * parent.sign  # > 0: market moved away, < 0: traded through
        if behind > 0:
            return
        if behind == 0 and rng.random() >= VENUES[venue].passive_fill_prob:
            return
        fill_qty = qty
        if qty >= 200 and rng.random() < 0.3:
            fill_qty = max(100, qty * rng.randrange(3, 9) // 10 // 100 * 100)
        record_fill(parent, child_id, venue, fill_qty, posted, ts)

    def plan_order(clock_s: int, offset_us: int, symbol, side, lots, broker, strategy) -> None:
        spec = STRATEGIES[strategy]
        number = len(data.orders) + 1
        parent = _Parent(
            f"P{number:04d}", symbol, 1 if side == "buy" else -1, lots * 100, broker, strategy
        )
        arrival_ts = SESSION_START + clock_s * US_PER_S + offset_us
        data.orders.append(
            {
                "order_id": parent.order_id,
                "symbol": symbol,
                "side": side,
                "qty": parent.qty,
                "decision_ts": arrival_ts - rng.randrange(200_000, 3_000_000),
                "arrival_ts": arrival_ts,
                "broker": broker,
                "strategy": strategy,
            }
        )
        duration = rng.randrange(*spec.duration_s)
        count = min(rng.randrange(spec.children[0], spec.children[1] + 1), lots)
        slice_qty = lots // count * 100
        last_second = clock_s
        for index in range(count):
            qty = slice_qty if index < count - 1 else parent.qty - slice_qty * (count - 1)
            second = clock_s + 1 + duration * index // count + rng.randrange(0, 3)
            venue = rng.choices(venue_names, weights=BROKERS[broker])[0]
            aggressive = rng.random() < spec.aggressive_prob
            schedule[second, symbol].append(
                (rng.randrange(US_PER_S), "child", (parent, venue, qty, aggressive))
            )
            last_second = max(last_second, second)
        if spec.cleanup:
            # Later than any passive check from the last child, so it cannot overfill.
            schedule[last_second + 10, symbol].append(
                (rng.randrange(US_PER_S), "cleanup", (parent,))
            )

    symbol_names = list(SYMBOLS)
    arrivals = []
    for _ in range(RANDOM_ORDERS):
        arrivals.append(
            (
                rng.randrange(90, SESSION_SECONDS - 800),
                rng.randrange(US_PER_S),
                rng.choice(symbol_names),
                rng.choice(("buy", "sell")),
                rng.choice(list(BROKERS)),
                rng.choice(list(STRATEGIES)),
            )
        )
    for clock, symbol, side, lots, broker, strategy in PLANTED_ORDERS:
        second = (_at(clock) - SESSION_START) // US_PER_S
        arrivals.append((second, 250_000, symbol, side, broker, strategy, lots))
    for arrival in sorted(arrivals, key=lambda a: (a[0], a[1], a[2])):
        second, offset_us, symbol, side, broker, strategy = arrival[:6]
        lots = arrival[6] if len(arrival) > 6 else rng.randrange(*SYMBOLS[symbol].lots)
        plan_order(second, offset_us, symbol, side, lots, broker, strategy)

    for second in range(SESSION_SECONDS):
        second_start = SESSION_START + second * US_PER_S
        for symbol, spec in SYMBOLS.items():
            move = rng.random()
            if move < spec.move_prob / 2:
                mids[symbol] -= 1
            elif move < spec.move_prob:
                mids[symbol] += 1

            events = schedule.pop((second, symbol), [])
            for venue in venue_names:
                if rng.random() < QUOTE_PROB:
                    events.append((rng.randrange(US_PER_S), "quote", (venue,)))
            # Stable sort: simultaneous events keep the order they were scheduled in.
            for offset_us, kind, payload in sorted(events, key=lambda event: event[0]):
                ts = second_start + offset_us
                if kind == "quote":
                    emit_quote(symbol, payload[0], ts)
                elif kind == "child":
                    parent, venue, qty, aggressive = payload
                    if aggressive:
                        send_aggressive(parent, venue, qty, ts)
                    else:
                        send_passive(parent, venue, qty, ts, second)
                elif kind == "passive_check":
                    check_passive(payload, ts)
                else:  # cleanup
                    (parent,) = payload
                    remaining = parent.qty - parent.filled
                    if remaining > 0:
                        venue = rng.choices(venue_names, weights=BROKERS[parent.broker])[0]
                        send_aggressive(parent, venue, remaining, ts)

            volume = rng.randrange(spec.max_volume)
            if volume:
                trade(symbol, second_start, mids[symbol] + rng.choice((-1, 0, 0, 1)), volume)

    for (symbol, minute), volume in bar_volume.items():
        if any(s == symbol and start <= minute < end for s, start, end in BAR_GAPS):
            continue
        vwap = Decimal(bar_notional[symbol, minute]) * TICK / volume
        data.bars.append(
            {
                "symbol": symbol,
                "ts": minute,
                "interval_s": BAR_SECONDS,
                "vwap": format(vwap, ".4f"),
                "volume": volume,
            }
        )

    captured: dict[tuple[str, str, int], dict] = {}
    for quote in data.quotes:
        true_ts = quote.pop("true_ts")
        if any(s == quote["symbol"] and start <= true_ts < end for s, start, end in QUOTE_GAPS):
            continue
        captured.setdefault((quote["symbol"], quote["venue"], quote["ts"]), quote)
    data.quotes = sorted(captured.values(), key=lambda q: (q["ts"], q["symbol"], q["venue"]))
    data.bars.sort(key=lambda b: (b["ts"], b["symbol"]))
    data.fills.sort(key=lambda f: (f["ts"], f["fill_id"]))
    data.child_orders.sort(key=lambda c: (c["sent_ts"], c["child_id"]))
    return data


def render_csv(dataset: str, rows: list[dict]) -> str:
    buffer = io.StringIO()
    writer = csv.writer(buffer, lineterminator="\n")
    columns = COLUMNS[dataset]
    writer.writerow(columns)
    for row in rows:
        writer.writerow(
            [
                (format_ts(row[column]) if column.endswith("ts") else row[column])
                if row[column] is not None
                else ""
                for column in columns
            ]
        )
    return buffer.getvalue()


def render_all(data: SimData) -> dict[str, str]:
    return {f"{name}.csv": render_csv(name, getattr(data, name)) for name in COLUMNS}


def write_fixture(out_dir: Path, seed: int = DEFAULT_SEED) -> dict[str, int]:
    out_dir.mkdir(parents=True, exist_ok=True)
    data = simulate(seed)
    for name, content in render_all(data).items():
        (out_dir / name).write_text(content, encoding="utf-8", newline="")
    return {name: len(getattr(data, name)) for name in COLUMNS}
