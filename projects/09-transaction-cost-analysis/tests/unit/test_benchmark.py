from decimal import Decimal as D

import pytest
from builders import at, bar, quote

from tca.benchmark import (
    DEGRADED,
    MISSING,
    OK,
    BarBook,
    Methodology,
    QuoteBook,
    interval_vwap,
    quote_benchmark,
)


def book(*quotes, **params) -> QuoteBook:
    return QuoteBook(quotes, Methodology(**params))


# -- point-in-time quote lookup -----------------------------------------------------------


def test_as_of_returns_the_latest_quote_at_or_before_the_instant():
    quotes = book(
        quote("100.00", "100.02", 0),
        quote("100.02", "100.04", 2),
        quote("100.04", "100.06", 4),
    )
    bbo = quotes.bbo_at("XYZ", at(3))
    assert (bbo.bid, bbo.ask, bbo.mid) == (D("100.02"), D("100.04"), D("100.03"))
    assert bbo.quote_ts == at(2) and bbo.age_us == 1_000_000
    assert bbo.status == OK


def test_a_quote_one_microsecond_in_the_future_is_invisible():
    quotes = book(quote("100.00", "100.02", 1))
    assert quotes.bbo_at("XYZ", at(0.999999)) is None
    assert quotes.bbo_at("XYZ", at(1)) is not None
    assert quotes.bbo_at("OTHER", at(5)) is None


def test_exclusive_lookup_ignores_a_quote_stamped_at_the_same_instant():
    quotes = book(quote("100.00", "100.02", 0), quote("100.02", "100.04", 2))
    assert quotes.bbo_at("XYZ", at(2)).mid == D("100.03")
    assert quotes.bbo_at("XYZ", at(2), inclusive=False).mid == D("100.01")


def test_venues_are_consolidated_into_a_best_bid_and_offer():
    quotes = book(
        quote("100.00", "100.06", 0, venue="V1"),
        quote("100.02", "100.08", 1, venue="V2"),
    )
    bbo = quotes.bbo_at("XYZ", at(2))
    assert (bbo.bid, bbo.bid_venue) == (D("100.02"), "V2")
    assert (bbo.ask, bbo.ask_venue) == (D("100.06"), "V1")
    assert bbo.mid == D("100.04")
    assert bbo.venues == ("V1", "V2")
    assert bbo.age_us == 2_000_000  # age of the older of the two quotes in use


def test_a_venue_that_stops_quoting_ages_out_of_the_consolidated_book():
    quotes = book(
        quote("100.03", "100.05", 0, venue="V1"),  # tight, but about to go silent
        quote("100.00", "100.10", 6, venue="V2"),
    )
    bbo = quotes.bbo_at("XYZ", at(7))
    assert (bbo.bid, bbo.ask, bbo.venues) == (D("100.00"), D("100.10"), ("V2",))
    assert not bbo.stale


def test_when_every_venue_is_stale_the_newest_quote_is_returned_and_marked():
    quotes = book(
        quote("100.03", "100.05", 0, venue="V1"),
        quote("100.00", "100.10", 1, venue="V2"),
    )
    bbo = quotes.bbo_at("XYZ", at(10))
    assert bbo.stale and bbo.status == DEGRADED
    assert (bbo.bid, bbo.ask, bbo.venues) == (D("100.00"), D("100.10"), ("V2",))
    assert bbo.age_us == 9_000_000


def test_staleness_boundary_is_inclusive_and_configurable():
    quotes = book(quote("100.00", "100.02", 0))
    assert not quotes.bbo_at("XYZ", at(5)).stale
    assert quotes.bbo_at("XYZ", at(5.000001)).stale
    strict = book(quote("100.00", "100.02", 0), max_quote_age_ms=1000)
    assert strict.bbo_at("XYZ", at(1.5)).stale


def test_a_crossed_consolidated_market_is_flagged_but_a_locked_one_is_not():
    crossed = book(
        quote("100.05", "100.07", 0, venue="V1"),
        quote("100.01", "100.03", 0, venue="V2"),
    ).bbo_at("XYZ", at(1))
    assert crossed.crossed and crossed.status == DEGRADED
    assert (crossed.bid, crossed.ask) == (D("100.05"), D("100.03"))

    locked = book(
        quote("100.03", "100.05", 0, venue="V1"),
        quote("100.01", "100.03", 0, venue="V2"),
    ).bbo_at("XYZ", at(1))
    assert not locked.crossed and locked.status == OK


def test_clock_skew_between_event_and_receive_time_is_flagged():
    normal = book(quote("100.00", "100.02", 0, recv=0.1)).bbo_at("XYZ", at(1))
    assert normal.clock_skew_us == 100_000 and not normal.skewed

    fast_venue_clock = book(quote("100.00", "100.02", 0, recv=-1.8)).bbo_at("XYZ", at(1))
    assert fast_venue_clock.clock_skew_us == 1_800_000
    assert fast_venue_clock.skewed and fast_venue_clock.status == DEGRADED

    unrecorded = book(quote("100.00", "100.02", 0)).bbo_at("XYZ", at(1))
    assert unrecorded.clock_skew_us is None and not unrecorded.skewed


def test_receive_clock_shows_only_what_had_arrived_locally():
    quotes = (
        quote("100.00", "100.02", 0, recv=0.001),
        quote("100.10", "100.12", 1, recv=3),  # delayed two seconds in transit
        quote("99.00", "99.02", 0.5, venue="V2"),  # no receive time: falls back to event time
    )
    by_event = QuoteBook(quotes, Methodology(quote_clock="event")).bbo_at("XYZ", at(2))
    by_receive = QuoteBook(quotes, Methodology(quote_clock="receive")).bbo_at("XYZ", at(2))
    assert by_event.bid == D("100.10")
    assert by_receive.bid == D("100.00") and by_receive.venues == ("V1", "V2")


def test_update_times_lists_every_venue_update_in_range():
    quotes = book(
        quote("100.00", "100.02", 0),
        quote("100.00", "100.02", 2, venue="V2"),
        quote("100.00", "100.02", 4),
    )
    assert quotes.update_times("XYZ", at(1), at(4)) == [at(2), at(4)]


def test_quote_benchmark_flags_are_namespaced_and_missing_has_no_value():
    quotes = book(quote("100.00", "100.04", 10))
    missing = quote_benchmark("arrival_mid", "ARRIVAL", quotes, "XYZ", at(9))
    assert (missing.value, missing.status) == (None, MISSING)
    assert missing.flags == ("ARRIVAL_QUOTE_MISSING",) and not missing.trusted

    stale = quote_benchmark("decision_mid", "DECISION", quotes, "XYZ", at(20))
    assert stale.value == D("100.02") and stale.status == DEGRADED
    assert stale.flags == ("DECISION_QUOTE_STALE",) and not stale.trusted
    assert stale.details["quote_age_us"] == 10_000_000

    ok = quote_benchmark("arrival_mid", "ARRIVAL", quotes, "XYZ", at(11))
    assert ok.trusted and ok.flags == ()
    assert ok.details["bid_venue"] == "V1" and ok.details["as_of_ts"] == at(11)


# -- interval VWAP ------------------------------------------------------------------------

BARS = (bar(0, "100.04", 12_000), bar(1, "100.09", 8_000))


def test_interval_vwap_weights_bars_by_volume():
    result = interval_vwap(BarBook(BARS), "XYZ", at(0), at(80))
    # (100.04 * 12,000 + 100.09 * 8,000) / 20,000
    assert result.value == D("100.06") and result.status == OK
    assert result.details["volume"] == 20_000
    assert (result.details["bars_expected"], result.details["bars_found"]) == (2, 2)
    assert (
        interval_vwap(BarBook((bar(0, "100", 1), bar(1, "200", 3))), "XYZ", at(0), at(80)).value
        == 175
    )


def test_window_is_rounded_outward_to_whole_bars_and_recorded():
    result = interval_vwap(BarBook(BARS), "XYZ", at(30), at(100))
    assert result.value == D("100.06")
    assert result.details["window_start_ts"] == at(0)
    assert result.details["window_end_ts"] == at(120)
    assert (result.details["order_start_ts"], result.details["order_end_ts"]) == (at(30), at(100))


def test_a_fill_exactly_on_a_bar_boundary_belongs_to_the_bar_that_starts_there():
    result = interval_vwap(BarBook(BARS), "XYZ", at(0), at(60))
    assert result.details["bars_expected"] == 2 and result.flags == ()


def test_missing_bars_inside_the_window_degrade_the_benchmark():
    result = interval_vwap(BarBook(BARS[:1]), "XYZ", at(0), at(80))
    assert result.value == D("100.04")
    assert result.status == DEGRADED and result.flags == ("VWAP_BARS_PARTIAL",)
    assert (result.details["bars_expected"], result.details["bars_found"]) == (2, 1)


@pytest.mark.parametrize(
    "bars",
    [(), (bar(0, "100.04", 0),), (bar(30, "100.04", 5_000),)],
    ids=["no bars for symbol", "zero volume", "bars outside window"],
)
def test_no_usable_volume_means_no_vwap(bars):
    result = interval_vwap(BarBook(bars), "XYZ", at(0), at(50))
    assert (result.value, result.status, result.flags) == (None, MISSING, ("VWAP_BARS_MISSING",))


def test_an_order_shorter_than_one_bar_is_flagged_as_coarse():
    short = interval_vwap(BarBook(BARS), "XYZ", at(10), at(30))
    assert short.value == D("100.04")
    assert short.flags == ("VWAP_BARS_COARSE",) and not short.trusted
    full_bar = interval_vwap(BarBook(BARS), "XYZ", at(0), at(60))
    assert "VWAP_BARS_COARSE" not in full_bar.flags


def test_an_interval_that_ends_before_it_starts_is_invalid():
    result = interval_vwap(BarBook(BARS), "XYZ", at(50), at(10))
    assert (result.value, result.flags) == (None, ("VWAP_WINDOW_INVALID",))


# -- methodology identity -----------------------------------------------------------------


def test_methodology_id_is_stable_and_changes_with_any_parameter():
    base = Methodology()
    assert base.id == Methodology().id
    assert base.id.startswith(f"{base.version}+")
    changed = {
        Methodology(max_quote_age_ms=1000).id,
        Methodology(max_clock_skew_ms=10).id,
        Methodology(quote_clock="receive").id,
        Methodology(version="tca-9.9").id,
    }
    assert len(changed) == 4 and base.id not in changed


def test_methodology_rejects_nonsense():
    with pytest.raises(ValueError):
        Methodology(quote_clock="wall")
    with pytest.raises(ValueError):
        Methodology(max_quote_age_ms=-1)
