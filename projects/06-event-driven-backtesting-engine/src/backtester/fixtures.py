"""Synthetic market data for demos, tests and benchmarks.

Everything here is invented. The generator uses integer arithmetic and a seeded stream
only (no floating point, no libm), so a given seed produces byte-identical data on every
platform. Prices follow a random walk with slowly alternating drift; that structure is
there to give example strategies something to do, and says nothing about real markets.

The simulated venue trades 14:30 to 21:00 UTC on weekdays and has no holidays.
"""

from __future__ import annotations

import csv
from collections.abc import Sequence
from math import isqrt
from pathlib import Path

from .data import DataError, Dataset
from .events import ActionType, Bar, CorporateAction, EventType, MarketEvent
from .exchange import derive_rng
from .fixedpoint import fmt_units, to_units
from .timeutil import NS_PER_DAY, NS_PER_SEC, day_index, fmt_ts, is_weekday, parse_ts

SESSION_OPEN_NS = (14 * 3600 + 30 * 60) * NS_PER_SEC
SESSION_CLOSE_NS = 21 * 3600 * NS_PER_SEC
_ONE = 1_000_000_000  # fixed-point scale for relative price moves
_BP = 100_000  # one basis point at that scale


def _normal(rng) -> int:
    """Roughly normal integer with mean 0 and standard deviation 1000 (Irwin-Hall)."""
    return sum(rng.randrange(1001) for _ in range(12)) - 6000


def synthetic_dataset(
    symbols: Sequence[str] = ("AAA", "BBB", "CCC"),
    start: str | int = "2023-01-02",
    days: int = 520,
    seed: int = 7,
    *,
    bar_seconds: int | None = None,
    price_decimals: int = 4,
    daily_vol_bps: int = 150,
    trend_bps: int = 8,
    daily_volume: int = 1_000_000,
    splits: Sequence[tuple[str, int, int, int]] = (),
    dividends: Sequence[tuple[str, int, str]] = (),
    gaps: Sequence[tuple[str, int, int]] = (),
) -> Dataset:
    """Generate ``days`` weekdays of bars per symbol.

    bar_seconds=None gives one bar per session; otherwise the session is cut into bars
    of that length. ``splits`` are (symbol, weekday index, num, den), ``dividends`` are
    (symbol, weekday index, cash per unit) and ``gaps`` are (symbol, first weekday index,
    last weekday index) ranges of bars to leave out.
    """
    if days <= 0:
        raise ValueError("days must be positive")
    session = SESSION_CLOSE_NS - SESSION_OPEN_NS
    bar_ns = session if bar_seconds is None else bar_seconds * NS_PER_SEC
    if bar_ns <= 0 or session % bar_ns:
        raise ValueError("bar_seconds must divide the 6.5 hour session exactly")
    bars_per_day = session // bar_ns

    first_day = day_index(parse_ts(start))
    trading_days: list[int] = []
    day = first_day
    while len(trading_days) < days:
        if is_weekday(day):
            trading_days.append(day)
        day += 1

    vol = daily_vol_bps * _BP * 1000 // isqrt(bars_per_day * 1_000_000)
    drift = trend_bps * _BP // bars_per_day
    gap_vol = daily_vol_bps * _BP // 3
    bar_volume = max(1, daily_volume // bars_per_day)

    events: list[MarketEvent] = []
    actions: list[CorporateAction] = []
    for symbol in symbols:
        rng = derive_rng(seed, f"fixture:{symbol}")
        price = to_units("100", price_decimals) * (60 + rng.randrange(81)) // 100
        volume = bar_volume
        direction = 1 if rng.randrange(2) else -1
        regime_left = 30 + rng.randrange(61)
        symbol_splits = {d: (n, m) for s, d, n, m in splits if s == symbol}
        symbol_dividends = {d: amount for s, d, amount in dividends if s == symbol}
        skipped = {d for s, lo, hi in gaps if s == symbol for d in range(lo, hi + 1)}

        for index, trading_day in enumerate(trading_days):
            midnight = trading_day * NS_PER_DAY
            if index in symbol_splits:
                num, den = symbol_splits[index]
                price = max(1, price * den // num)
                volume = max(1, volume * num // den)
                actions.append(CorporateAction.split(midnight, symbol, num, den))
            if index in symbol_dividends:
                amount = to_units(symbol_dividends[index], price_decimals)
                actions.append(CorporateAction.dividend(midnight, symbol, amount))
            if regime_left == 0:
                direction = -direction
                regime_left = 30 + rng.randrange(61)
            regime_left -= 1

            price = max(1, price * (_ONE + gap_vol * _normal(rng) // 1000) // _ONE)
            for slot in range(bars_per_day):
                open_px = price
                move = direction * drift + vol * _normal(rng) // 1000
                close_px = max(1, open_px * (_ONE + move) // _ONE)
                top, bottom = max(open_px, close_px), min(open_px, close_px)
                high = top + top * (vol * abs(_normal(rng)) // 2000) // _ONE
                low = max(1, bottom - bottom * (vol * abs(_normal(rng)) // 2000) // _ONE)
                traded = volume * (500 + rng.randrange(1001)) // 1000
                price = close_px
                # Draw every bar even when it is skipped, so a gap never shifts later data.
                if index in skipped:
                    continue
                ts_open = midnight + SESSION_OPEN_NS + slot * bar_ns
                bar = Bar(ts_open, ts_open + bar_ns, open_px, high, low, close_px, traded)
                events.append(MarketEvent.bar(symbol, bar))
    return Dataset.build(events, actions, price_decimals=price_decimals)


def demo_dataset(seed: int = 7, days: int = 520) -> Dataset:
    """Three symbols of daily bars with a split, a dividend and a four-day hole."""
    return synthetic_dataset(
        ("AAA", "BBB", "CCC"),
        days=days,
        seed=seed,
        splits=(("BBB", 300, 2, 1),) if days > 300 else (),
        dividends=(("AAA", 200, "0.50"),) if days > 200 else (),
        gaps=(("CCC", 250, 253),) if days > 253 else (),
    )


# -- writers ----------------------------------------------------------------------------


def write_bars(dataset: Dataset, path: str | Path) -> Path:
    """Write the dataset's bars to ``.csv`` or ``.parquet``, chosen by file suffix."""
    target = Path(path)
    bars = [e for e in dataset.events if e.event_type is EventType.BAR]
    decimals = dataset.price_decimals
    target.parent.mkdir(parents=True, exist_ok=True)
    suffix = target.suffix.lower()
    if suffix == ".csv":
        with target.open("w", newline="", encoding="utf-8") as handle:
            writer = csv.writer(handle, lineterminator="\n")
            writer.writerow(
                [
                    "symbol",
                    "ts_open",
                    "ts_close",
                    "ts_avail",
                    "open",
                    "high",
                    "low",
                    "close",
                    "volume",
                ]
            )
            for e in bars:
                b = e.payload
                writer.writerow(
                    [
                        e.symbol,
                        fmt_ts(b.ts_open),
                        fmt_ts(b.ts_close),
                        fmt_ts(e.ts),
                        fmt_units(b.open, decimals),
                        fmt_units(b.high, decimals),
                        fmt_units(b.low, decimals),
                        fmt_units(b.close, decimals),
                        b.volume,
                    ]
                )
    elif suffix in (".parquet", ".pq"):
        try:
            import pyarrow as pa
            import pyarrow.parquet as pq
        except ImportError as exc:  # pragma: no cover - depends on the environment
            raise DataError("writing Parquet needs pyarrow: pip install pyarrow") from exc
        scale = 10**decimals
        stamp = pa.timestamp("ns", tz="UTC")
        table = pa.table(
            {
                "symbol": pa.array([e.symbol for e in bars], pa.string()),
                "ts_open": pa.array([e.payload.ts_open for e in bars], pa.int64()).cast(stamp),
                "ts_close": pa.array([e.payload.ts_close for e in bars], pa.int64()).cast(stamp),
                "ts_avail": pa.array([e.ts for e in bars], pa.int64()).cast(stamp),
                "open": pa.array([e.payload.open / scale for e in bars], pa.float64()),
                "high": pa.array([e.payload.high / scale for e in bars], pa.float64()),
                "low": pa.array([e.payload.low / scale for e in bars], pa.float64()),
                "close": pa.array([e.payload.close / scale for e in bars], pa.float64()),
                "volume": pa.array([e.payload.volume for e in bars], pa.int64()),
            }
        )
        pq.write_table(table, target)
    else:
        raise DataError(f"unsupported output type {target.suffix!r}; use .csv or .parquet")
    return target


def write_corporate_actions(dataset: Dataset, path: str | Path) -> Path:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    with target.open("w", newline="", encoding="utf-8") as handle:
        writer = csv.writer(handle, lineterminator="\n")
        writer.writerow(["ts", "symbol", "type", "value"])
        for action in dataset.corporate_actions:
            if action.action_type is ActionType.SPLIT:
                value = f"{action.ratio_num}:{action.ratio_den}"
            else:
                value = fmt_units(action.amount, dataset.price_decimals)
            writer.writerow([fmt_ts(action.ts), action.symbol, action.action_type.value, value])
    return target
