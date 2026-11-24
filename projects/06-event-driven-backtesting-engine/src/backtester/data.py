"""Point-in-time historical datasets.

The loader turns CSV or Parquet files into validated, canonically ordered events and
refuses data that would let a strategy see something before it was published. The
dataset's ``data_version`` is a hash of its normalised content, so the same data hashes
the same whether it came from CSV or Parquet and whatever order its rows were in.
"""

from __future__ import annotations

import csv
import hashlib
from collections import Counter
from collections.abc import Iterable, Sequence
from dataclasses import dataclass
from decimal import Decimal, InvalidOperation
from functools import cached_property
from pathlib import Path
from typing import Any

from .events import ActionType, Bar, CorporateAction, EventType, MarketEvent, Quote
from .fixedpoint import to_units
from .timeutil import NS_PER_DAY, NS_PER_SEC, day_index, fmt_ts, parse_ts, weekdays_between

CALENDARS = ("weekdays", "24x7")

_TS_OPEN = ("ts_open", "timestamp", "ts", "datetime", "date", "time")
_TS_EVENT = ("ts_event", "timestamp", "ts", "datetime", "time")
_TS_AVAIL = ("ts_avail", "ts_recv", "ts_publish", "available_at")


class DataError(ValueError):
    """The dataset is malformed or violates a point-in-time invariant."""


@dataclass(frozen=True, slots=True)
class Gap:
    """A hole in one symbol's bars, between the bars opening at ``after`` and ``before``."""

    symbol: str
    after: int
    before: int
    missing: int
    unit: str  # "bars" or "weekdays"


@dataclass(frozen=True, slots=True)
class SymbolSummary:
    symbol: str
    event_type: EventType
    count: int
    first_ts: int
    last_ts: int
    interval_ns: int | None
    gaps: tuple[Gap, ...]


@dataclass(frozen=True, slots=True)
class DataSummary:
    symbols: tuple[SymbolSummary, ...]
    first_ts: int | None
    last_ts: int | None
    event_count: int
    corporate_actions: tuple[CorporateAction, ...]
    price_decimals: int
    duplicates_dropped: int
    invalid_dropped: int
    calendar: str
    sources: tuple[tuple[str, str], ...]

    @property
    def gap_count(self) -> int:
        return sum(len(s.gaps) for s in self.symbols)


class Dataset:
    """Validated market events in canonical order, plus corporate actions."""

    def __init__(
        self,
        events: Sequence[MarketEvent],
        corporate_actions: Sequence[CorporateAction] = (),
        *,
        price_decimals: int = 4,
        calendar: str = "weekdays",
        duplicates_dropped: int = 0,
        invalid_dropped: int = 0,
        sources: Sequence[tuple[str, str]] = (),
    ) -> None:
        if calendar not in CALENDARS:
            raise DataError(f"unknown calendar {calendar!r}; expected one of {CALENDARS}")
        self.events: tuple[MarketEvent, ...] = tuple(events)
        self.corporate_actions: tuple[CorporateAction, ...] = tuple(corporate_actions)
        self.price_decimals = price_decimals
        self.calendar = calendar
        self.duplicates_dropped = duplicates_dropped
        self.invalid_dropped = invalid_dropped
        self.sources = tuple(sources)

    @classmethod
    def build(
        cls,
        events: Iterable[MarketEvent],
        corporate_actions: Iterable[CorporateAction] = (),
        *,
        price_decimals: int = 4,
        calendar: str = "weekdays",
        on_invalid: str = "raise",
        sources: Sequence[tuple[str, str]] = (),
    ) -> Dataset:
        """Validate, canonically order and de-duplicate raw events.

        on_invalid="raise" stops at the first bad row. "drop" removes bad rows and
        counts them, so the report can disclose how many were discarded.
        """
        if on_invalid not in ("raise", "drop"):
            raise DataError("on_invalid must be 'raise' or 'drop'")
        valid: list[MarketEvent] = []
        invalid = 0
        for event in events:
            problem = _validate(event)
            if problem is None:
                valid.append(event)
            elif on_invalid == "raise":
                raise DataError(f"{event.symbol} @ {fmt_ts(event.ts_event)}: {problem}")
            else:
                invalid += 1

        # Stable sort: distinct quotes that share a venue timestamp keep their file order.
        valid.sort(key=lambda e: (e.ts_event, e.symbol, e.event_type, e.ts))
        deduped: list[MarketEvent] = []
        seen_bars: dict[tuple[str, int], MarketEvent] = {}
        seen_quotes: set[MarketEvent] = set()
        duplicates = 0
        kinds: dict[str, EventType] = {}
        for event in valid:
            kind = kinds.setdefault(event.symbol, event.event_type)
            if kind is not event.event_type:
                raise DataError(
                    f"{event.symbol} has both bars and quotes; a symbol needs exactly one "
                    "source of fills"
                )
            if event.event_type is EventType.BAR:
                key = (event.symbol, event.payload.ts_open)
                first = seen_bars.get(key)
                if first is not None:
                    if first == event:
                        duplicates += 1
                        continue
                    raise DataError(
                        f"{event.symbol}: two different bars open at {fmt_ts(key[1])}. "
                        "Revised data is not supported; keep the version first published"
                    )
                seen_bars[key] = event
            elif event in seen_quotes:
                duplicates += 1
                continue
            else:
                seen_quotes.add(event)
            deduped.append(event)

        actions = sorted(
            set(corporate_actions), key=lambda a: (a.ts, a.symbol, a.action_type, a.amount)
        )
        for action in actions:
            _validate_action(action)
        return cls(
            deduped,
            actions,
            price_decimals=price_decimals,
            calendar=calendar,
            duplicates_dropped=duplicates,
            invalid_dropped=invalid,
            sources=sources,
        )

    def __len__(self) -> int:
        return len(self.events)

    @cached_property
    def symbols(self) -> tuple[str, ...]:
        return tuple(sorted({e.symbol for e in self.events}))

    @cached_property
    def first_ts(self) -> int | None:
        """Start of the earliest bar or quote."""
        starts = [_start(e) for e in self.events]
        return min(starts) if starts else None

    @cached_property
    def last_ts(self) -> int | None:
        """Latest availability time."""
        return max((e.ts for e in self.events), default=None)

    def slice(self, start: int | None = None, end: int | None = None) -> Dataset:
        """Events that become available in [start, end). Used for train/test separation."""
        events = [
            e
            for e in self.events
            if (start is None or e.ts >= start) and (end is None or e.ts < end)
        ]
        actions = [
            a
            for a in self.corporate_actions
            if (start is None or a.ts >= start) and (end is None or a.ts < end)
        ]
        return Dataset(
            events,
            actions,
            price_decimals=self.price_decimals,
            calendar=self.calendar,
            duplicates_dropped=self.duplicates_dropped,
            invalid_dropped=self.invalid_dropped,
            sources=self.sources,
        )

    @cached_property
    def data_version(self) -> str:
        digest = hashlib.sha256()
        digest.update(f"backtester-data/1|decimals={self.price_decimals}\n".encode())
        lines: list[str] = []
        for e in self.events:
            p = e.payload
            if e.event_type is EventType.BAR:
                lines.append(
                    f"B|{e.symbol}|{p.ts_open}|{p.ts_close}|{e.ts}|"
                    f"{p.open}|{p.high}|{p.low}|{p.close}|{p.volume}"
                )
            else:
                lines.append(
                    f"Q|{e.symbol}|{e.ts_event}|{e.ts}|{p.bid}|{p.ask}|{p.bid_size}|{p.ask_size}"
                )
            if len(lines) >= 50_000:
                digest.update(("\n".join(lines) + "\n").encode())
                lines.clear()
        for a in self.corporate_actions:
            lines.append(
                f"C|{a.symbol}|{a.ts}|{a.action_type}|{a.ratio_num}|{a.ratio_den}|{a.amount}"
            )
        digest.update(("\n".join(lines) + "\n").encode())
        return "sha256:" + digest.hexdigest()

    @cached_property
    def summary(self) -> DataSummary:
        by_symbol: dict[str, list[MarketEvent]] = {}
        for event in self.events:
            by_symbol.setdefault(event.symbol, []).append(event)
        symbols = []
        for symbol in sorted(by_symbol):
            events = by_symbol[symbol]
            kind = events[0].event_type
            interval: int | None = None
            gaps: tuple[Gap, ...] = ()
            if kind is EventType.BAR:
                opens = [e.payload.ts_open for e in events]
                interval = _modal_interval(opens)
                if interval:
                    gaps = _find_gaps(symbol, opens, interval, self.calendar)
            symbols.append(
                SymbolSummary(
                    symbol=symbol,
                    event_type=kind,
                    count=len(events),
                    first_ts=min(_start(e) for e in events),
                    last_ts=max(e.ts_event for e in events),
                    interval_ns=interval,
                    gaps=gaps,
                )
            )
        return DataSummary(
            symbols=tuple(symbols),
            first_ts=self.first_ts,
            last_ts=self.last_ts,
            event_count=len(self.events),
            corporate_actions=self.corporate_actions,
            price_decimals=self.price_decimals,
            duplicates_dropped=self.duplicates_dropped,
            invalid_dropped=self.invalid_dropped,
            calendar=self.calendar,
            sources=self.sources,
        )


# -- validation -------------------------------------------------------------------------


def _start(event: MarketEvent) -> int:
    return event.payload.ts_open if event.event_type is EventType.BAR else event.ts_event


def _validate(event: MarketEvent) -> str | None:
    if not event.symbol:
        return "empty symbol"
    if event.ts < event.ts_event:
        return (
            f"published at {fmt_ts(event.ts)}, before it happened. Publication cannot "
            "precede the event; check for clock skew or timezone errors"
        )
    p = event.payload
    if event.event_type is EventType.BAR:
        if not isinstance(p, Bar):
            return "bar event without a Bar payload"
        if p.ts_close != event.ts_event:
            return "bar venue time must equal its close time"
        if p.ts_close <= p.ts_open:
            return "bar must close after it opens"
        if min(p.open, p.high, p.low, p.close) <= 0:
            return "bar prices must be positive"
        if p.high < max(p.open, p.close) or p.low > min(p.open, p.close):
            return "bar high/low do not contain its open and close"
        if p.volume < 0:
            return "bar volume must not be negative"
    else:
        if not isinstance(p, Quote):
            return "quote event without a Quote payload"
        if min(p.bid, p.ask, p.bid_size, p.ask_size) < 0:
            return "quote prices and sizes must not be negative"
        if p.bid > 0 and p.ask > 0 and p.bid > p.ask:
            return "crossed quote (bid above ask)"
    return None


def _validate_action(action: CorporateAction) -> None:
    if action.action_type is ActionType.SPLIT:
        if action.ratio_num <= 0 or action.ratio_den <= 0:
            raise DataError(f"{action.symbol}: split ratio must be positive")
    elif action.amount <= 0:
        raise DataError(f"{action.symbol}: dividend amount must be positive")


def _modal_interval(opens: list[int]) -> int | None:
    deltas = Counter(b - a for a, b in zip(opens, opens[1:], strict=False))
    if not deltas:
        return None
    best = max(deltas.values())
    return min(delta for delta, count in deltas.items() if count == best)


def _find_gaps(symbol: str, opens: list[int], interval: int, calendar: str) -> tuple[Gap, ...]:
    """Heuristic gap detection; no exchange holiday or session calendar is consulted.

    24x7: any spacing wider than the usual interval is a gap. weekdays: missing bars
    within a UTC day, and whole weekdays with no bars, are gaps (so holidays are listed).
    """
    gaps: list[Gap] = []
    for a, b in zip(opens, opens[1:], strict=False):
        if b - a <= interval:
            continue
        if calendar == "24x7":
            gaps.append(Gap(symbol, a, b, (b - a) // interval - 1, "bars"))
            continue
        day_a, day_b = day_index(a), day_index(b)
        if day_a == day_b:
            if interval < NS_PER_DAY:
                gaps.append(Gap(symbol, a, b, (b - a) // interval - 1, "bars"))
            continue
        missing = weekdays_between(day_a, day_b)
        if missing:
            gaps.append(Gap(symbol, a, b, missing, "weekdays"))
    return tuple(gaps)


# -- file loading -----------------------------------------------------------------------


def load_dataset(
    bars: str | Path | None = None,
    quotes: str | Path | None = None,
    corporate_actions: str | Path | None = None,
    *,
    price_decimals: int = 4,
    bar_seconds: int | None = None,
    publish_delay_ns: int = 0,
    default_symbol: str | None = None,
    calendar: str = "weekdays",
    on_invalid: str = "raise",
) -> Dataset:
    """Load bars and/or quotes (CSV or Parquet) and optional corporate actions (CSV).

    Bars need ``ts_open``, ``open``, ``high``, ``low``, ``close``; ``ts_close`` may be
    replaced by ``bar_seconds``. ``ts_avail`` defaults to the close plus ``publish_delay_ns``.
    """
    if bars is None and quotes is None:
        raise DataError("no market data given: pass a bars file, a quotes file, or both")
    events: list[MarketEvent] = []
    sources: list[tuple[str, str]] = []
    if bars is not None:
        events.extend(
            _load_bars(Path(bars), price_decimals, bar_seconds, publish_delay_ns, default_symbol)
        )
        sources.append((Path(bars).name, _file_sha256(Path(bars))))
    if quotes is not None:
        events.extend(_load_quotes(Path(quotes), price_decimals, publish_delay_ns, default_symbol))
        sources.append((Path(quotes).name, _file_sha256(Path(quotes))))
    actions: list[CorporateAction] = []
    if corporate_actions is not None:
        actions = load_corporate_actions(Path(corporate_actions), price_decimals)
        sources.append((Path(corporate_actions).name, _file_sha256(Path(corporate_actions))))
    return Dataset.build(
        events,
        actions,
        price_decimals=price_decimals,
        calendar=calendar,
        on_invalid=on_invalid,
        sources=sources,
    )


def load_corporate_actions(path: Path, price_decimals: int = 4) -> list[CorporateAction]:
    """CSV with columns ts, symbol, type (split|dividend), value ("2:1" or cash per unit)."""
    columns, count = _read_columns(path)
    ts = _column(columns, ("ts", "timestamp", "date", "ex_date"), path)
    symbols = _column(columns, ("symbol",), path)
    kinds = _column(columns, ("type", "action", "action_type"), path)
    values = _column(columns, ("value", "ratio", "amount"), path)
    actions = []
    for i in range(count):
        try:
            kind = ActionType(str(kinds[i]).strip().lower())
            when = parse_ts(ts[i])
            symbol = str(symbols[i]).strip()
            if kind is ActionType.SPLIT:
                num, _, den = str(values[i]).replace("/", ":").partition(":")
                actions.append(CorporateAction.split(when, symbol, int(num), int(den or 1)))
            else:
                amount = to_units(_amount(values[i]), price_decimals)
                actions.append(CorporateAction.dividend(when, symbol, amount))
        except (ValueError, TypeError) as exc:
            raise DataError(f"{path.name} row {i + 1}: {exc}") from exc
    return actions


def _load_bars(
    path: Path,
    decimals: int,
    bar_seconds: int | None,
    publish_delay_ns: int,
    default_symbol: str | None,
) -> list[MarketEvent]:
    columns, count = _read_columns(path)
    try:
        opens = [parse_ts(v) for v in _column(columns, _TS_OPEN, path)]
        if "ts_close" in columns:
            closes = [parse_ts(v) for v in columns["ts_close"]]
        elif bar_seconds:
            closes = [t + bar_seconds * NS_PER_SEC for t in opens]
        else:
            raise DataError(f"{path.name}: no ts_close column; pass bar_seconds to derive it")
        avail_values = _optional(columns, _TS_AVAIL)
        if avail_values is None:
            avail = [t + publish_delay_ns for t in closes]
        else:
            avail = [parse_ts(v) for v in avail_values]
        prices = {
            name: [to_units(_amount(v), decimals) for v in _column(columns, (name,), path)]
            for name in ("open", "high", "low", "close")
        }
        volume_values = _optional(columns, ("volume", "vol"))
        if volume_values is None:
            volumes = [0] * count
        else:
            volumes = [_quantity(v) for v in volume_values]
    except (ValueError, TypeError) as exc:
        if isinstance(exc, DataError):
            raise
        raise DataError(f"{path.name}: {exc}") from exc
    symbols = _symbols(columns, count, default_symbol, path)
    return [
        MarketEvent.bar(
            symbols[i],
            Bar(
                opens[i],
                closes[i],
                prices["open"][i],
                prices["high"][i],
                prices["low"][i],
                prices["close"][i],
                volumes[i],
            ),
            avail[i],
        )
        for i in range(count)
    ]


def _load_quotes(
    path: Path, decimals: int, publish_delay_ns: int, default_symbol: str | None
) -> list[MarketEvent]:
    columns, count = _read_columns(path)
    try:
        ts_event = [parse_ts(v) for v in _column(columns, _TS_EVENT, path)]
        avail_values = _optional(columns, _TS_AVAIL)
        if avail_values is None:
            avail = [t + publish_delay_ns for t in ts_event]
        else:
            avail = [parse_ts(v) for v in avail_values]
        bids = [to_units(_amount(v), decimals) for v in _column(columns, ("bid",), path)]
        asks = [to_units(_amount(v), decimals) for v in _column(columns, ("ask",), path)]
        bid_sizes = [_quantity(v) for v in _column(columns, ("bid_size",), path)]
        ask_sizes = [_quantity(v) for v in _column(columns, ("ask_size",), path)]
    except (ValueError, TypeError) as exc:
        if isinstance(exc, DataError):
            raise
        raise DataError(f"{path.name}: {exc}") from exc
    symbols = _symbols(columns, count, default_symbol, path)
    return [
        MarketEvent.quote(
            symbols[i], ts_event[i], Quote(bids[i], asks[i], bid_sizes[i], ask_sizes[i]), avail[i]
        )
        for i in range(count)
    ]


def _read_columns(path: Path) -> tuple[dict[str, list[Any]], int]:
    """Read a CSV or Parquet file into lower-cased column lists."""
    if not path.exists():
        raise DataError(f"data file not found: {path}")
    suffix = path.suffix.lower()
    if suffix in (".parquet", ".pq"):
        return _read_parquet(path)
    if suffix in (".csv", ".txt"):
        with path.open(newline="", encoding="utf-8") as handle:
            reader = csv.reader(handle)
            try:
                header = [name.strip().lower() for name in next(reader)]
            except StopIteration:
                raise DataError(f"{path.name} is empty") from None
            columns: dict[str, list[Any]] = {name: [] for name in header}
            count = 0
            for line, row in enumerate(reader, start=2):
                if not row:
                    continue
                if len(row) != len(header):
                    raise DataError(
                        f"{path.name} line {line}: expected {len(header)} fields, got {len(row)}"
                    )
                for name, value in zip(header, row, strict=True):
                    columns[name].append(value)
                count += 1
        return columns, count
    raise DataError(f"unsupported data file type {path.suffix!r}; use .csv or .parquet")


def _read_parquet(path: Path) -> tuple[dict[str, list[Any]], int]:
    try:
        import pyarrow as pa
        import pyarrow.parquet as pq
    except ImportError as exc:  # pragma: no cover - depends on the environment
        raise DataError("reading Parquet needs pyarrow: pip install pyarrow") from exc
    try:
        table = pq.read_table(path)
    except (pa.ArrowException, OSError) as exc:
        raise DataError(f"{path.name}: cannot read Parquet file: {exc}") from exc
    columns: dict[str, list[Any]] = {}
    for name in table.column_names:
        column = table.column(name)
        if pa.types.is_timestamp(column.type):
            # Timezone-naive timestamps are taken to be UTC.
            column = column.cast(pa.timestamp("ns", tz=column.type.tz)).cast(pa.int64())
        elif pa.types.is_date(column.type):
            column = column.cast(pa.timestamp("ns")).cast(pa.int64())
        columns[name.strip().lower()] = column.to_pylist()
    return columns, table.num_rows


def _column(columns: dict[str, list[Any]], names: Sequence[str], path: Path) -> list[Any]:
    values = _optional(columns, names)
    if values is None:
        raise DataError(
            f"{path.name}: missing column {names[0]!r} (have: {', '.join(sorted(columns))})"
        )
    return values


def _optional(columns: dict[str, list[Any]], names: Sequence[str]) -> list[Any] | None:
    for name in names:
        if name in columns:
            return columns[name]
    return None


def _symbols(
    columns: dict[str, list[Any]], count: int, default_symbol: str | None, path: Path
) -> list[str]:
    if "symbol" in columns:
        return [str(v).strip() for v in columns["symbol"]]
    if default_symbol:
        return [default_symbol] * count
    raise DataError(f"{path.name}: no symbol column; pass a default symbol")


def _amount(value: Any) -> Any:
    if value is None or (isinstance(value, str) and not value.strip()):
        raise ValueError("missing price")
    return value


def _quantity(value: Any) -> int:
    if value is None or (isinstance(value, str) and not value.strip()):
        raise ValueError("missing quantity")
    if isinstance(value, int) and not isinstance(value, bool):
        return value
    try:
        number = Decimal(str(value).strip())
    except InvalidOperation:
        raise ValueError(f"unparseable quantity {value!r}") from None
    if not number.is_finite() or number != number.to_integral_value():
        raise ValueError(f"quantity {value!r} is not a whole number of units")
    return int(number)


def _file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()
