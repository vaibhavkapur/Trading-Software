"""Normalising loaders for the five input datasets.

Rows are validated one at a time. A bad row is rejected with a reason code and the rest of
the batch still loads; a payload whose shape is wrong (unknown dataset, missing or
misspelt columns, unparseable body) fails as a whole with ``ImportFormatError``.
"""

from __future__ import annotations

import csv
import io
import json
from collections.abc import Callable, Iterable, Mapping
from dataclasses import dataclass, field
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Any

from tca.domain import DATASETS, Bar, ChildOrder, Fill, ParentOrder, Quote, Side
from tca.timeutil import US_PER_S, parse_ts


class ImportFormatError(ValueError):
    """The payload as a whole cannot be interpreted."""


class RowError(ValueError):
    def __init__(self, code: str, detail: str):
        super().__init__(detail)
        self.code = code
        self.detail = detail


@dataclass(frozen=True)
class Reject:
    row: int  # 1-based data row (the CSV header is not counted)
    code: str
    detail: str


@dataclass
class ParsedBatch:
    dataset: str
    rows: int = 0
    records: list[tuple[int, Any]] = field(default_factory=list)
    rejects: list[Reject] = field(default_factory=list)


Row = Mapping[str, Any]


def _blank(value: Any) -> bool:
    return value is None or (isinstance(value, str) and value.strip() == "")


def _text(row: Row, name: str) -> str:
    value = row.get(name)
    if _blank(value):
        raise RowError("MISSING_FIELD", f"{name} is required")
    return str(value).strip()


def _opt_text(row: Row, name: str) -> str | None:
    value = row.get(name)
    return None if _blank(value) else str(value).strip()


def _decimal(row: Row, name: str) -> Decimal:
    value = row.get(name)
    if _blank(value):
        raise RowError("MISSING_FIELD", f"{name} is required")
    if isinstance(value, bool):
        raise RowError("BAD_NUMBER", f"{name} is not a number: {value!r}")
    try:
        # repr() of a float is its shortest round-trip form, so 100.1 stays 100.1.
        number = Decimal(repr(value) if isinstance(value, float) else str(value).strip())
    except InvalidOperation as exc:
        raise RowError("BAD_NUMBER", f"{name} is not a number: {value!r}") from exc
    if not number.is_finite():
        raise RowError("BAD_NUMBER", f"{name} is not finite: {value!r}")
    return number


def _positive_decimal(row: Row, name: str) -> Decimal:
    number = _decimal(row, name)
    if number <= 0:
        raise RowError("NON_POSITIVE", f"{name} must be > 0, got {number}")
    return number


def _integer(row: Row, name: str, *, minimum: int) -> int:
    number = _decimal(row, name)
    if number != number.to_integral_value():
        raise RowError("NOT_WHOLE", f"{name} must be a whole number, got {number}")
    if number < minimum:
        raise RowError("OUT_OF_RANGE", f"{name} must be >= {minimum}, got {number}")
    return int(number)


def _timestamp(row: Row, name: str) -> int:
    value = row.get(name)
    if _blank(value):
        raise RowError("MISSING_FIELD", f"{name} is required")
    try:
        return parse_ts(value)
    except ValueError as exc:
        raise RowError("BAD_TIMESTAMP", f"{name}: {exc}") from exc


def _opt_timestamp(row: Row, name: str) -> int | None:
    return None if _blank(row.get(name)) else _timestamp(row, name)


def _order(row: Row) -> ParentOrder:
    side_text = _text(row, "side").lower()
    if side_text not in (Side.BUY, Side.SELL):
        raise RowError("BAD_SIDE", f"side must be buy or sell, got {side_text!r}")
    decision_ts = _timestamp(row, "decision_ts")
    arrival_ts = _timestamp(row, "arrival_ts")
    if decision_ts > arrival_ts:
        raise RowError("DECISION_AFTER_ARRIVAL", "decision_ts is later than arrival_ts")
    return ParentOrder(
        order_id=_text(row, "order_id"),
        symbol=_text(row, "symbol"),
        side=Side(side_text),
        qty=_integer(row, "qty", minimum=1),
        decision_ts=decision_ts,
        arrival_ts=arrival_ts,
        broker=_text(row, "broker"),
        strategy=_text(row, "strategy"),
    )


def _child_order(row: Row) -> ChildOrder:
    return ChildOrder(
        child_id=_text(row, "child_id"),
        order_id=_text(row, "order_id"),
        venue=_text(row, "venue"),
        qty=_integer(row, "qty", minimum=1),
        sent_ts=_timestamp(row, "sent_ts"),
    )


def _fill(row: Row) -> Fill:
    return Fill(
        fill_id=_text(row, "fill_id"),
        order_id=_text(row, "order_id"),
        child_id=_opt_text(row, "child_id"),
        venue=_text(row, "venue"),
        qty=_integer(row, "qty", minimum=1),
        price=_positive_decimal(row, "price"),
        fee=_decimal(row, "fee"),
        ts=_timestamp(row, "ts"),
    )


def _quote(row: Row) -> Quote:
    bid = _positive_decimal(row, "bid")
    ask = _positive_decimal(row, "ask")
    if bid > ask:
        raise RowError("CROSSED_QUOTE", f"bid {bid} is above ask {ask}")
    return Quote(
        symbol=_text(row, "symbol"),
        venue=_text(row, "venue"),
        bid=bid,
        ask=ask,
        ts=_timestamp(row, "ts"),
        recv_ts=_opt_timestamp(row, "recv_ts"),
    )


def _bar(row: Row) -> Bar:
    ts = _timestamp(row, "ts")
    interval_s = _integer(row, "interval_s", minimum=1)
    if ts % (interval_s * US_PER_S) != 0:
        raise RowError("BAR_MISALIGNED", f"bar start is not a multiple of {interval_s}s")
    return Bar(
        symbol=_text(row, "symbol"),
        ts=ts,
        interval_s=interval_s,
        vwap=_positive_decimal(row, "vwap"),
        volume=_integer(row, "volume", minimum=0),
    )


@dataclass(frozen=True)
class DatasetSpec:
    required: tuple[str, ...]
    optional: tuple[str, ...]
    build: Callable[[Row], Any]

    @property
    def columns(self) -> tuple[str, ...]:
        return self.required + self.optional


SPECS: dict[str, DatasetSpec] = {
    "orders": DatasetSpec(
        (
            "order_id",
            "symbol",
            "side",
            "qty",
            "decision_ts",
            "arrival_ts",
            "broker",
            "strategy",
        ),
        (),
        _order,
    ),
    "child_orders": DatasetSpec(
        ("child_id", "order_id", "venue", "qty", "sent_ts"), (), _child_order
    ),
    "fills": DatasetSpec(
        ("fill_id", "order_id", "venue", "qty", "price", "fee", "ts"), ("child_id",), _fill
    ),
    "quotes": DatasetSpec(("symbol", "venue", "bid", "ask", "ts"), ("recv_ts",), _quote),
    "bars": DatasetSpec(("symbol", "ts", "interval_s", "vwap", "volume"), (), _bar),
}
assert tuple(SPECS) == DATASETS


def spec_for(dataset: str) -> DatasetSpec:
    try:
        return SPECS[dataset]
    except KeyError:
        raise ImportFormatError(
            f"unknown dataset {dataset!r}; expected one of {', '.join(DATASETS)}"
        ) from None


def _check_columns(spec: DatasetSpec, columns: Iterable[str]) -> None:
    seen = set(columns)
    missing = [name for name in spec.required if name not in seen]
    unknown = sorted(seen - set(spec.columns))
    if missing:
        raise ImportFormatError(f"missing required columns: {', '.join(missing)}")
    if unknown:
        # Refuse rather than ignore: a misspelt optional column would silently load as null.
        raise ImportFormatError(f"unknown columns: {', '.join(unknown)}")


def parse_rows(dataset: str, rows: Iterable[Row]) -> ParsedBatch:
    """Validate already-tabular rows (dicts keyed by column name)."""
    spec = spec_for(dataset)
    batch = ParsedBatch(dataset)
    for number, row in enumerate(rows, start=1):
        batch.rows = number
        if not isinstance(row, Mapping):
            batch.rejects.append(Reject(number, "MALFORMED_ROW", "row is not an object"))
            continue
        try:
            _check_columns(spec, row.keys())
            batch.records.append((number, spec.build(row)))
        except ImportFormatError as exc:
            batch.rejects.append(Reject(number, "MALFORMED_ROW", str(exc)))
        except RowError as exc:
            batch.rejects.append(Reject(number, exc.code, exc.detail))
    return batch


def parse_csv(dataset: str, text: str) -> ParsedBatch:
    spec = spec_for(dataset)
    reader = csv.DictReader(io.StringIO(text.lstrip("\ufeff")))
    if reader.fieldnames is None:
        raise ImportFormatError("empty payload: a CSV header row is required")
    header = [name.strip() for name in reader.fieldnames]
    if len(set(header)) != len(header):
        raise ImportFormatError("duplicate column names in header")
    _check_columns(spec, header)
    reader.fieldnames = header

    batch = ParsedBatch(dataset)
    for number, row in enumerate(reader, start=1):
        batch.rows = number
        if None in row or any(value is None for value in row.values()):
            batch.rejects.append(Reject(number, "MALFORMED_ROW", f"expected {len(header)} fields"))
            continue
        try:
            batch.records.append((number, spec.build(row)))
        except RowError as exc:
            batch.rejects.append(Reject(number, exc.code, exc.detail))
    return batch


def parse_json(dataset: str, body: str | bytes) -> ParsedBatch:
    """Parse ``{"rows": [...]}`` or a bare list. Numbers are read as ``Decimal`` so a JSON
    price never passes through binary floating point."""
    try:
        payload = json.loads(body, parse_float=Decimal)
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise ImportFormatError(f"body is not valid JSON: {exc}") from exc
    rows = payload.get("rows") if isinstance(payload, dict) else payload
    if not isinstance(rows, list):
        raise ImportFormatError('expected a JSON array of rows or {"rows": [...]}')
    return parse_rows(dataset, rows)


def parse_file(dataset: str, path: Path) -> ParsedBatch:
    """Load a ``.csv`` or ``.parquet`` file. Parquet needs the optional ``pyarrow`` extra."""
    suffix = path.suffix.lower()
    if suffix == ".csv":
        return parse_csv(dataset, path.read_text(encoding="utf-8"))
    if suffix == ".parquet":
        try:
            import pyarrow.parquet as pq
        except ImportError as exc:
            raise ImportFormatError(
                'reading Parquet needs pyarrow: pip install "tca[parquet]"'
            ) from exc
        return parse_rows(dataset, pq.read_table(path).to_pylist())
    raise ImportFormatError(f"unsupported file type {suffix!r}; use .csv or .parquet")


def find_dataset_files(directory: Path) -> dict[str, Path]:
    """Map dataset name to ``<dataset>.csv`` or ``<dataset>.parquet`` inside a directory."""
    found: dict[str, Path] = {}
    for dataset in DATASETS:
        for suffix in (".csv", ".parquet"):
            candidate = directory / f"{dataset}{suffix}"
            if candidate.is_file():
                found[dataset] = candidate
                break
    return found
