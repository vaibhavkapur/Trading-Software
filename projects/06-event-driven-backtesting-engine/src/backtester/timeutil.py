"""Timestamps are integer nanoseconds since the Unix epoch, always UTC.

Integers keep ordering exact and platform independent. Naive inputs are assumed to be UTC.
"""

from __future__ import annotations

import re
from datetime import UTC, date, datetime, timedelta

NS_PER_US = 1_000
NS_PER_MS = 1_000_000
NS_PER_SEC = 1_000_000_000
NS_PER_DAY = 86_400 * NS_PER_SEC

_EPOCH = datetime(1970, 1, 1, tzinfo=UTC)
_NAIVE_EPOCH = datetime(1970, 1, 1)
_INTEGER = re.compile(r"-?\d+")
_FRACTION = re.compile(r"\.(\d+)")


def _from_datetime(dt: datetime) -> int:
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=UTC)
    delta = dt - _EPOCH
    return (delta.days * 86_400 + delta.seconds) * NS_PER_SEC + delta.microseconds * NS_PER_US


def parse_ts(value: int | str | datetime | date) -> int:
    """Convert an epoch-nanosecond integer, ISO-8601 string, datetime or date to UTC ns."""
    if isinstance(value, bool):
        raise TypeError("a boolean is not a timestamp")
    if isinstance(value, int):
        return value
    if isinstance(value, datetime):
        return _from_datetime(value)
    if isinstance(value, date):
        return _from_datetime(datetime(value.year, value.month, value.day, tzinfo=UTC))

    text = str(value).strip()
    if _INTEGER.fullmatch(text):
        return int(text)

    # datetime.fromisoformat truncates beyond microseconds, so carry the fraction separately.
    frac_ns = 0
    match = _FRACTION.search(text)
    if match:
        digits = match.group(1)
        if len(digits) > 9:
            raise ValueError(f"timestamp {value!r} has sub-nanosecond precision")
        frac_ns = int(digits.ljust(9, "0"))
        text = text[: match.start()] + text[match.end() :]
    try:
        parsed = datetime.fromisoformat(text)
    except ValueError as exc:
        raise ValueError(f"unparseable timestamp {value!r}") from exc
    return _from_datetime(parsed) + frac_ns


def fmt_ts(ns: int) -> str:
    """Render UTC nanoseconds as ISO-8601 with a trailing Z."""
    seconds, frac = divmod(ns, NS_PER_SEC)
    # isoformat, not strftime: this is called once per equity row and strftime is slow.
    text = (_NAIVE_EPOCH + timedelta(seconds=seconds)).isoformat()
    if frac:
        text += f".{frac:09d}".rstrip("0")
    return text + "Z"


def fmt_date(ns: int) -> str:
    return (_NAIVE_EPOCH + timedelta(days=day_index(ns))).date().isoformat()


def day_index(ns: int) -> int:
    """Whole UTC days since the epoch."""
    return ns // NS_PER_DAY


def is_weekday(day: int) -> bool:
    """True for Monday to Friday. Day 0 (1970-01-01) was a Thursday."""
    return (day + 3) % 7 < 5


def weekdays_between(first_day: int, last_day: int) -> int:
    """Count weekdays strictly between two day indexes."""
    return sum(1 for day in range(first_day + 1, last_day) if is_weekday(day))
