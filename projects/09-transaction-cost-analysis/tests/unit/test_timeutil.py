from datetime import UTC, datetime, timedelta, timezone

import pytest

from tca.timeutil import format_ts, parse_ts


def test_offsets_resolve_to_the_same_instant():
    utc = parse_ts("2026-03-02T15:00:00Z")
    assert parse_ts("2026-03-02T15:00:00+00:00") == utc
    assert parse_ts("2026-03-02T10:00:00-05:00") == utc
    assert parse_ts("2026-03-02T20:30:00+05:30") == utc


def test_microseconds_survive_a_round_trip():
    ts = parse_ts("2026-03-02T15:00:00.123456Z")
    assert ts % 1_000_000 == 123_456
    assert format_ts(ts) == "2026-03-02T15:00:00.123456Z"


def test_epoch_is_zero():
    assert parse_ts("1970-01-01T00:00:00Z") == 0
    assert format_ts(0) == "1970-01-01T00:00:00.000000Z"


def test_aware_datetimes_are_accepted():
    aware = datetime(2026, 3, 2, 10, 0, tzinfo=timezone(timedelta(hours=-5)))
    assert parse_ts(aware) == parse_ts(datetime(2026, 3, 2, 15, 0, tzinfo=UTC))


@pytest.mark.parametrize(
    "value",
    ["2026-03-02T15:00:00", "2026-03-02 15:00:00", datetime(2026, 3, 2, 15, 0)],
)
def test_naive_timestamps_are_rejected(value):
    with pytest.raises(ValueError, match="no UTC offset"):
        parse_ts(value)


@pytest.mark.parametrize("value", ["", "yesterday", "15:00", 1772463600, None])
def test_non_timestamps_are_rejected(value):
    with pytest.raises(ValueError):
        parse_ts(value)


def test_format_none_is_none():
    assert format_ts(None) is None
