"""Timestamps are integer microseconds since the Unix epoch, always UTC.

Inputs must carry an explicit UTC offset. A naive timestamp is rejected rather than guessed
at, because a silent local-time assumption shifts every benchmark lookup.
"""

from __future__ import annotations

from datetime import UTC, datetime, timedelta

_EPOCH = datetime(1970, 1, 1, tzinfo=UTC)

US_PER_MS = 1_000
US_PER_S = 1_000_000


def parse_ts(value: object) -> int:
    """Parse an ISO-8601 string or aware datetime into UTC microseconds."""
    if isinstance(value, datetime):
        dt = value
    elif isinstance(value, str):
        try:
            dt = datetime.fromisoformat(value.strip())
        except ValueError as exc:
            raise ValueError(f"not an ISO-8601 timestamp: {value!r}") from exc
    else:
        raise ValueError(f"not a timestamp: {value!r}")
    if dt.utcoffset() is None:
        raise ValueError(f"timestamp has no UTC offset: {value!r}")
    delta = dt - _EPOCH
    return (delta.days * 86_400 + delta.seconds) * US_PER_S + delta.microseconds


def format_ts(ts_us: int | None) -> str | None:
    """Render UTC microseconds as ``2026-03-02T14:30:00.000000Z``."""
    if ts_us is None:
        return None
    dt = _EPOCH + timedelta(microseconds=ts_us)
    return dt.strftime("%Y-%m-%dT%H:%M:%S.%f") + "Z"
