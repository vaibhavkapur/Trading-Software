import random
from datetime import UTC, date, datetime, timedelta, timezone
from decimal import Decimal
from fractions import Fraction

import pytest

from backtester.fixedpoint import Ratio, div_round_half_even, fmt_units, to_units
from backtester.timeutil import (
    NS_PER_DAY,
    NS_PER_SEC,
    day_index,
    fmt_date,
    fmt_ts,
    is_weekday,
    parse_ts,
    weekdays_between,
)

# -- timestamps -------------------------------------------------------------------------


def test_parse_ts_accepts_the_documented_forms():
    expected = 1_704_205_800 * NS_PER_SEC  # 2024-01-02T14:30:00Z
    assert parse_ts("2024-01-02T14:30:00Z") == expected
    assert parse_ts("2024-01-02 14:30:00") == expected  # naive means UTC
    assert parse_ts("2024-01-02T09:30:00-05:00") == expected
    assert parse_ts(datetime(2024, 1, 2, 14, 30, tzinfo=UTC)) == expected
    assert parse_ts(datetime(2024, 1, 2, 9, 30, tzinfo=timezone(timedelta(hours=-5)))) == expected
    assert parse_ts(expected) == expected
    assert parse_ts(str(expected)) == expected
    assert parse_ts("2024-01-02") == parse_ts(date(2024, 1, 2)) == expected - 52_200 * NS_PER_SEC


def test_parse_ts_keeps_nanosecond_precision():
    assert parse_ts("2024-01-02T14:30:00.123456789Z") % NS_PER_SEC == 123_456_789
    assert parse_ts("2024-01-02T14:30:00.5Z") % NS_PER_SEC == 500_000_000


@pytest.mark.parametrize("bad", ["", "yesterday", "2024-13-01", "2024-01-02T14:30:00.1234567891Z"])
def test_parse_ts_rejects_garbage_and_sub_nanosecond_input(bad):
    with pytest.raises(ValueError):
        parse_ts(bad)


def test_parse_ts_rejects_booleans():
    with pytest.raises(TypeError):
        parse_ts(True)


@pytest.mark.parametrize(
    "text",
    ["1970-01-01T00:00:00Z", "2024-02-29T23:59:59Z", "2024-01-02T14:30:00.000000001Z"],
)
def test_fmt_ts_round_trips(text):
    assert fmt_ts(parse_ts(text)) == text


def test_calendar_helpers():
    monday = day_index(parse_ts("2024-01-01"))
    assert [is_weekday(monday + i) for i in range(7)] == [True] * 5 + [False] * 2
    assert weekdays_between(monday + 4, monday + 7) == 0  # Friday to Monday
    assert weekdays_between(monday, monday + 4) == 3
    assert fmt_date(monday * NS_PER_DAY + 5) == "2024-01-01"


# -- fixed point ------------------------------------------------------------------------


def test_to_units_is_exact_for_decimal_text():
    assert to_units("101.25", 4) == 1_012_500
    assert to_units(Decimal("0.0001"), 4) == 1
    assert to_units(7, 4) == 70_000
    assert to_units("-3.5", 2) == -350
    assert to_units(" 12 ", 0) == 12


def test_to_units_rounds_half_even_at_the_last_unit():
    assert to_units("0.00005", 4) == 0
    assert to_units("0.00015", 4) == 2
    assert to_units("0.00025", 4) == 2


def test_to_units_recovers_the_intended_value_from_a_float():
    # 123.4567 has no exact binary representation; the boundary conversion must not drift.
    assert to_units(123.4567, 4) == 1_234_567
    assert to_units(0.1 + 0.2, 4) == 3_000


@pytest.mark.parametrize("bad", ["abc", "nan", "inf", float("nan"), float("inf")])
def test_to_units_rejects_non_numbers(bad):
    with pytest.raises(ValueError):
        to_units(bad, 4)


def test_fmt_units_round_trips_any_integer():
    rng = random.Random(1)
    for decimals in (0, 2, 4, 8):
        for _ in range(200):
            units = rng.randrange(-(10**12), 10**12)
            assert to_units(fmt_units(units, decimals), decimals) == units
    assert fmt_units(-5, 4) == "-0.0005"
    assert fmt_units(1_012_500, 4) == "101.2500"
    assert fmt_units(42, 0) == "42"


def test_div_round_half_even_matches_exact_rational_rounding():
    rng = random.Random(2)
    for _ in range(2_000):
        n = rng.randrange(-(10**9), 10**9)
        d = rng.choice([-1, 1]) * rng.randrange(1, 10**4)
        assert div_round_half_even(n, d) == round(Fraction(n, d))
    assert [div_round_half_even(n, 2) for n in (-5, -3, -1, 1, 3, 5)] == [-2, -2, 0, 0, 2, 2]
    with pytest.raises(ZeroDivisionError):
        div_round_half_even(1, 0)


def test_ratio_rounds_in_the_direction_asked():
    three_bps = Ratio.from_bps("3")
    assert (three_bps.num, three_bps.den) == (3, 10_000)
    assert three_bps.ceil(12_345) == 4  # 3.7035
    assert three_bps.floor(12_345) == 3
    assert three_bps.ceil(10_000) == three_bps.floor(10_000) == 3  # exact: no rounding
    assert Ratio.from_bps("0.5").ceil(1_000_000) == 50
    assert not Ratio.from_bps(0)
    assert Ratio.parse("0.1").floor(999) == 99


def test_ratio_rejects_negative_and_unparseable_rates():
    with pytest.raises(ValueError):
        Ratio.from_bps("-1")
    with pytest.raises(ValueError):
        Ratio.parse("lots")
