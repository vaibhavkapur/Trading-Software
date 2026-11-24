import random

import pytest
from helpers import DAY, HOUR, MINUTE, T0, daily_bar, dataset, day_open, minute_bar, px, quote

from backtester import Bar, CorporateAction, DataError, Dataset, EventType, MarketEvent, Quote
from backtester.fixtures import synthetic_dataset


def weekday_bars(symbol="X", days=range(12)) -> list[MarketEvent]:
    """Bars on Monday to Friday only (day 0 is a Monday)."""
    return [daily_bar(symbol, d, 100, 101, 99, 100) for d in days if d % 7 < 5]


# -- point-in-time validation -----------------------------------------------------------


def test_data_published_before_it_happened_is_refused():
    skewed = daily_bar("X", 0, 100, 101, 99, 100, delay=-1)
    with pytest.raises(DataError, match="before it happened"):
        dataset([skewed])


@pytest.mark.parametrize(
    ("ohlc", "message"),
    [
        ((100, 99, 98, 99), "high/low"),  # high below open
        ((100, 101, 100.5, 100), "high/low"),  # low above open and close
        ((0, 1, 0, 1), "positive"),
        ((-1, 1, -1, 1), "positive"),
    ],
)
def test_impossible_bars_are_refused(ohlc, message):
    with pytest.raises(DataError, match=message):
        dataset([daily_bar("X", 0, *ohlc)])


def test_bar_must_close_after_it_opens_and_volume_cannot_be_negative():
    with pytest.raises(DataError, match="close after"):
        dataset([MarketEvent.bar("X", Bar(100, 100, 1, 1, 1, 1, 0))])
    with pytest.raises(DataError, match="volume"):
        dataset([daily_bar("X", 0, 100, 101, 99, 100, volume=-1)])


def test_crossed_or_negative_quotes_are_refused():
    with pytest.raises(DataError, match="crossed"):
        dataset([quote("X", T0, 101, 100)])
    with pytest.raises(DataError, match="negative"):
        dataset([MarketEvent.quote("X", T0, Quote(px(100), px(101), -1, 5))])
    assert len(dataset([quote("X", T0, 100, 0, ask_size=0)])) == 1  # one-sided is fine


def test_drop_mode_removes_bad_rows_and_counts_them():
    events = [daily_bar("X", 0, 100, 101, 99, 100), daily_bar("X", 1, 100, 99, 98, 99)]
    ds = dataset(events, on_invalid="drop")
    assert len(ds) == 1 and ds.invalid_dropped == 1 and ds.summary.invalid_dropped == 1


def test_a_symbol_cannot_mix_bars_and_quotes():
    with pytest.raises(DataError, match="both bars and quotes"):
        dataset([daily_bar("X", 0, 100, 101, 99, 100), quote("X", T0, 100, 101)])
    mixed = dataset([daily_bar("X", 0, 100, 101, 99, 100), quote("Y", T0, 100, 101)])
    assert {s.symbol: s.event_type for s in mixed.summary.symbols} == {
        "X": EventType.BAR,
        "Y": EventType.QUOTE,
    }


def test_corporate_actions_are_validated():
    with pytest.raises(DataError, match="split ratio"):
        dataset([], [CorporateAction.split(T0, "X", 0, 1)])
    with pytest.raises(DataError, match="dividend"):
        dataset([], [CorporateAction.dividend(T0, "X", 0)])


def test_unknown_calendar_is_refused():
    with pytest.raises(DataError, match="calendar"):
        dataset([], calendar="lunar")


# -- canonical order, duplicates and versioning -----------------------------------------


def test_row_order_does_not_change_the_dataset_or_its_version():
    events = weekday_bars("A") + weekday_bars("B")
    shuffled = events[:]
    random.Random(4).shuffle(shuffled)
    a, b = dataset(events), dataset(shuffled)
    assert a.events == b.events
    assert a.data_version == b.data_version
    keys = [(e.ts_event, e.symbol) for e in a.events]
    assert keys == sorted(keys)


def test_exact_duplicates_are_dropped_and_counted():
    events = weekday_bars()
    ds = dataset(events + events[:3])
    assert len(ds) == len(events)
    assert ds.duplicates_dropped == 3
    assert ds.data_version == dataset(events).data_version


def test_duplicate_quotes_are_dropped_but_distinct_quotes_at_one_instant_are_kept_in_order():
    first, second = quote("X", T0, 100, 101), quote("X", T0, 100, 102)
    ds = dataset([first, second, first])
    assert ds.events == (first, second)
    assert dataset([second, first]).events == (second, first)


def test_two_different_bars_for_the_same_time_are_refused():
    with pytest.raises(DataError, match="two different bars"):
        dataset([daily_bar("X", 0, 100, 101, 99, 100), daily_bar("X", 0, 100, 101, 99, 100.5)])


def test_any_change_to_the_data_changes_the_version():
    base = weekday_bars()
    version = dataset(base).data_version
    changed = base[:-1] + [daily_bar("X", 11, 100, 101, 99, 100.0001)]
    assert dataset(changed).data_version != version
    assert dataset(base[:-1]).data_version != version
    with_action = dataset(base, [CorporateAction.split(T0, "X", 2, 1)])
    assert with_action.data_version != version
    delayed = [daily_bar("X", d, 100, 101, 99, 100, delay=1) for d in range(12) if d % 7 < 5]
    assert dataset(delayed).data_version != version
    assert Dataset.build(base, price_decimals=2).data_version != version


# -- slicing ----------------------------------------------------------------------------


def test_slice_selects_by_availability_time_half_open():
    events = [daily_bar("X", d, 100, 101, 99, 100) for d in range(5)]
    ds = dataset(events)
    cut = events[2].ts
    assert [e.ts for e in ds.slice(end=cut).events] == [events[0].ts, events[1].ts]
    assert [e.ts for e in ds.slice(start=cut).events] == [e.ts for e in events[2:]]
    assert ds.slice(cut, cut).events == ()


def test_a_bar_published_late_belongs_to_the_slice_in_which_it_became_known():
    late = daily_bar("X", 0, 100, 101, 99, 100, delay=3 * DAY)
    ds = dataset([late, daily_bar("X", 1, 100, 101, 99, 100)])
    before = ds.slice(end=late.ts)
    assert late not in before.events and len(before) == 1


def test_slice_keeps_corporate_actions_inside_the_window():
    actions = [CorporateAction.split(T0 + d * DAY, "X", 2, 1) for d in (1, 3)]
    ds = dataset(weekday_bars(), actions)
    assert ds.slice(T0 + 2 * DAY, T0 + 5 * DAY).corporate_actions == (actions[1],)


# -- summary and gap detection ----------------------------------------------------------


def test_weekends_are_not_gaps_for_daily_bars():
    ds = dataset(weekday_bars(days=range(19)))
    summary = ds.summary.symbols[0]
    assert summary.interval_ns == DAY
    assert summary.gaps == ()


def test_missing_weekdays_are_reported_as_a_gap():
    days = [d for d in range(19) if d not in (8, 9)]  # Tuesday and Wednesday of week two
    (gap,) = dataset(weekday_bars(days=days)).summary.symbols[0].gaps
    assert (gap.after, gap.before, gap.missing, gap.unit) == (
        day_open(7),
        day_open(10),
        2,
        "weekdays",
    )


def test_a_24x7_calendar_treats_every_missing_interval_as_a_gap():
    events = [daily_bar("X", d, 100, 101, 99, 100) for d in (0, 1, 2, 3, 4, 7, 8)]
    (gap,) = dataset(events, calendar="24x7").summary.symbols[0].gaps
    assert (gap.missing, gap.unit) == (2, "bars")


def test_intraday_gaps_within_a_day_and_missing_days_are_both_reported():
    bars = [minute_bar("X", i, 100, 101, 99, 100) for i in range(10) if i not in (4, 5, 6)]
    next_day = 3 * DAY // MINUTE  # Thursday; Tuesday and Wednesday have nothing
    bars += [minute_bar("X", next_day + i, 100, 101, 99, 100) for i in range(3)]
    intraday, missing_days = dataset(bars).summary.symbols[0].gaps
    assert (intraday.missing, intraday.unit) == (3, "bars")
    assert (missing_days.missing, missing_days.unit) == (2, "weekdays")


def test_summary_reports_ranges_and_counts():
    ds = dataset(weekday_bars("A") + weekday_bars("B", days=range(5)))
    summary = ds.summary
    assert summary.event_count == 15 and [s.count for s in summary.symbols] == [10, 5]
    assert summary.first_ts == day_open(0)
    assert summary.last_ts == day_open(11) + int(6.5 * HOUR)
    assert ds.symbols == ("A", "B")


def test_empty_dataset_is_harmless():
    ds = dataset([])
    assert (len(ds), ds.first_ts, ds.last_ts, ds.symbols) == (0, None, None, ())
    assert ds.summary.symbols == () and ds.data_version.startswith("sha256:")


# -- synthetic fixtures -----------------------------------------------------------------


def test_fixture_is_a_pure_function_of_its_seed():
    assert synthetic_dataset(days=40, seed=5).events == synthetic_dataset(days=40, seed=5).events
    assert synthetic_dataset(days=40, seed=5).events != synthetic_dataset(days=40, seed=6).events


def test_adding_a_symbol_or_a_gap_does_not_change_other_data():
    one = synthetic_dataset(("AAA",), days=40, seed=5)
    two = synthetic_dataset(("AAA", "BBB"), days=40, seed=5)
    assert [e for e in two.events if e.symbol == "AAA"] == list(one.events)

    holed = synthetic_dataset(("AAA",), days=40, seed=5, gaps=(("AAA", 10, 12),))
    kept = {e.payload.ts_open for e in holed.events}
    assert len(holed) == 37
    assert [e for e in one.events if e.payload.ts_open in kept] == list(holed.events)


def test_fixture_splits_halve_the_price_and_emit_the_action():
    plain = synthetic_dataset(("AAA",), days=20, seed=5)
    split = synthetic_dataset(("AAA",), days=20, seed=5, splits=(("AAA", 10, 2, 1),))
    assert len(split.corporate_actions) == 1
    assert split.events[9] == plain.events[9]
    ratio = plain.events[10].payload.open / split.events[10].payload.open
    assert ratio == pytest.approx(2, rel=1e-3)


def test_intraday_fixture_has_contiguous_bars_within_the_session():
    ds = synthetic_dataset(("AAA",), days=2, seed=5, bar_seconds=1800)
    assert len(ds) == 26  # 13 half-hour bars per 6.5 hour session
    first_day = [e.payload for e in ds.events[:13]]
    assert all(
        a.ts_close == b.ts_open and a.close == b.open
        for a, b in zip(first_day, first_day[1:], strict=False)
    )
    with pytest.raises(ValueError):
        synthetic_dataset(days=1, bar_seconds=7_000)
