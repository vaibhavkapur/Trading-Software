"""Missing data, clock skew, stale events, feed loss, duplicate delivery, rejected writes."""

import os
import random
from pathlib import Path

import pytest
from helpers import DAY, HOUR, Recorder, daily_bar, dataset, day_close, day_open, px
from momentum import Momentum

from backtester import (
    BacktestConfig,
    DataError,
    OrderStatus,
    RiskLimits,
    Strategy,
    load_dataset,
    run_backtest,
)
from backtester.cli import EXIT_FAILED, EXIT_OK, EXIT_USAGE, main
from backtester.fixtures import demo_dataset, write_bars, write_corporate_actions
from backtester.report import render_report, write_artifacts

MOMENTUM = str(Path(__file__).resolve().parents[2] / "examples" / "momentum.py")


@pytest.fixture(scope="module")
def demo():
    return demo_dataset(seed=7, days=120)


@pytest.fixture
def files(tmp_path, demo):
    bars = write_bars(demo, tmp_path / "bars.csv")
    return tmp_path, bars


def rewrite(path: Path, edit) -> None:
    header, *rows = path.read_text().splitlines()
    path.write_text("\n".join([header, *edit(rows)]) + "\n")


# -- missing data and feed loss ---------------------------------------------------------


def gapped_bars() -> list:
    """Days 0-2, then nothing until day 8."""
    days = [0, 1, 2, 8, 9, 10]
    return [daily_bar("X", d, 100 + d, 101 + d, 99 + d, 100 + d) for d in days]


def test_an_order_does_not_fill_while_the_feed_is_silent():
    strategy = Recorder(
        {2: lambda ctx: ctx.submit_order("X", "buy", 10)}
    )  # last bar before the gap
    result = run_backtest(dataset(gapped_bars()), strategy, BacktestConfig())
    (fill,) = result.fills
    assert fill.ts == day_close(8) and fill.px == px(108)  # first open after the gap
    assert result.orders[0].submitted_at == day_close(2)
    assert not [s for s in result.snapshots if day_close(2) < s.ts < day_close(8)]


def test_positions_are_valued_at_the_last_published_price_through_a_gap():
    seen = []

    class Watcher(Strategy):
        def on_start(self, context):
            for day in range(3, 8):
                context.set_timer(day_open(day), "check")

        def on_market_event(self, event, context):
            if context.now == day_close(0):
                context.submit_order("X", "buy", 10)

        def on_timer(self, timer, context):
            seen.append((context.mark("X"), context.position("X")))

    run_backtest(dataset(gapped_bars()), Watcher(), BacktestConfig())
    assert seen == [(px(102), 10)] * 5


def test_stale_data_guard_rejects_orders_during_the_gap_only():
    class Daily(Strategy):
        def on_start(self, context):
            for day in range(1, 11):
                context.set_timer(day_open(day) - HOUR, f"day{day}")

        def on_timer(self, timer, context):
            context.submit_order("X", "buy", 1, tag=timer.name)

    guarded = BacktestConfig(risk=RiskLimits(max_data_age_ns=2 * DAY))
    result = run_backtest(dataset(gapped_bars()), Daily(), guarded)
    outcome = {o.tag: o.reason for o in result.orders}
    # The last bar before the gap closes on day 2 at 21:00; each order is sent at 13:30.
    # Day 4 is 40.5 hours later (inside the limit); day 8's bar has not been published yet.
    stale = [tag for tag, reason in outcome.items() if reason == "STALE_MARKET_DATA"]
    assert stale == ["day5", "day6", "day7", "day8"]
    assert all(outcome[f"day{d}"] is None for d in (1, 2, 3, 4, 9, 10))


def test_the_gap_is_disclosed_in_the_report():
    result = run_backtest(dataset(gapped_bars(), calendar="24x7"), Strategy(), BacktestConfig())
    report = render_report(result)
    assert "Data gaps (1):" in report
    assert "X: 5 missing bars between 2024-01-03T14:30:00Z and 2024-01-09T14:30:00Z" in report


def test_a_symbol_that_stops_publishing_leaves_its_orders_open():
    events = [daily_bar("LIVE", d, 100, 101, 99, 100) for d in range(6)]
    events += [daily_bar("DEAD", d, 50, 51, 49, 50) for d in range(2)]

    def enter(ctx):
        if ctx.mark("DEAD"):
            ctx.submit_order("DEAD", "buy", 5)

    # Event index 3 is DEAD's last bar (order: DEAD, LIVE, DEAD, LIVE, ...).
    result = run_backtest(dataset(events), Recorder({2: enter}), BacktestConfig())
    (order,) = result.orders
    assert order.symbol == "DEAD" and order.status is OrderStatus.WORKING
    assert result.fills == ()


# -- clock skew -------------------------------------------------------------------------


def skew_one_row(rows: list[str]) -> list[str]:
    fields = rows[10].split(",")
    fields[3] = fields[1]  # ts_avail := ts_open, hours before the bar closed
    rows[10] = ",".join(fields)
    return rows


def test_a_file_with_a_skewed_clock_is_refused(files, capsys):
    tmp, bars = files
    rewrite(bars, skew_one_row)
    with pytest.raises(DataError, match="before it happened"):
        load_dataset(bars)
    code = main(["run", "--strategy", MOMENTUM, "--data", str(bars), "--out", str(tmp / "run")])
    assert code == EXIT_USAGE
    assert "Publication cannot precede the event" in capsys.readouterr().err
    assert not (tmp / "run").exists()


def test_dropping_the_skewed_row_is_explicit_and_disclosed(files, capsys):
    tmp, bars = files
    rewrite(bars, skew_one_row)
    out = tmp / "run"
    code = main(
        [
            "run",
            "--strategy",
            MOMENTUM,
            "--data",
            str(bars),
            "--on-invalid",
            "drop",
            "--out",
            str(out),
        ]
    )
    assert code == EXIT_OK
    assert "Invalid rows dropped: 1." in (out / "report.md").read_text()


def test_a_timezone_error_shows_up_as_skew_not_as_a_silent_shift(tmp_path):
    # ts_avail written in local time (UTC-5) but read as UTC lands before the close.
    path = tmp_path / "bars.csv"
    path.write_text(
        "symbol,ts_open,ts_close,ts_avail,open,high,low,close,volume\n"
        "X,2024-01-02T14:30:00Z,2024-01-02T21:00:00Z,2024-01-02T16:00:00,100,101,99,100,10\n"
    )
    with pytest.raises(DataError, match="clock skew or timezone"):
        load_dataset(path)


# -- duplicate and out-of-order delivery ------------------------------------------------


def test_duplicated_shuffled_rows_give_the_same_run_as_clean_data(files):
    tmp, bars = files
    clean = load_dataset(bars)

    def scramble(rows: list[str]) -> list[str]:
        rows = rows + rows[::7]
        random.Random(5).shuffle(rows)
        return rows

    rewrite(bars, scramble)
    messy = load_dataset(bars)
    assert messy.duplicates_dropped == len(clean.events[::7]) > 0
    assert messy.data_version == clean.data_version

    a = run_backtest(clean, Momentum(lookback=10), BacktestConfig())
    b = run_backtest(messy, Momentum(lookback=10), BacktestConfig())
    assert a.result_hash == b.result_hash and a.fills
    assert f"Exact duplicate rows dropped: {messy.duplicates_dropped}." in render_report(b)


def test_a_conflicting_duplicate_is_refused_rather_than_guessed(files):
    _, bars = files

    def revise(rows: list[str]) -> list[str]:
        fields = rows[3].split(",")
        fields[7] = fields[5]  # close := high, still a valid bar
        return [*rows, ",".join(fields)]

    rewrite(bars, revise)
    with pytest.raises(DataError, match="two different bars"):
        load_dataset(bars)


# -- malformed files --------------------------------------------------------------------


@pytest.mark.parametrize(
    ("content", "message"),
    [
        ("", "empty"),
        (
            "symbol,ts_open,ts_close,open,high,low\nX,2024-01-02,2024-01-03,1,1,1\n",
            "missing column 'close'",
        ),
        ("symbol,ts_open,open,high,low,close\nX,2024-01-02,1,1,1,1\n", "bar_seconds"),
        (
            "symbol,ts_open,ts_close,open,high,low,close\nX,soon,2024-01-03,1,1,1,1\n",
            "unparseable timestamp",
        ),
        (
            "symbol,ts_open,ts_close,open,high,low,close\nX,2024-01-02,2024-01-03,1,1,1,\n",
            "missing price",
        ),
        (
            "symbol,ts_open,ts_close,open,high,low,close\nX,2024-01-02,2024-01-03,1,1,1,abc\n",
            "unparseable amount",
        ),
        (
            "symbol,ts_open,ts_close,open,high,low,close\nX,2024-01-02,2024-01-03,1,1,1\n",
            "expected 7 fields",
        ),
        (
            "ts_open,ts_close,open,high,low,close\n2024-01-02,2024-01-03,1,1,1,1\n",
            "no symbol column",
        ),
        (
            "symbol,ts_open,ts_close,open,high,low,close,volume\nX,2024-01-02,2024-01-03,1,1,1,1,2.5\n",
            "whole number",
        ),
    ],
)
def test_malformed_files_fail_with_a_message_that_says_what_is_wrong(tmp_path, content, message):
    path = tmp_path / "bars.csv"
    path.write_text(content)
    with pytest.raises(DataError, match=message):
        load_dataset(path)


def test_missing_and_unsupported_files(tmp_path):
    with pytest.raises(DataError, match="not found"):
        load_dataset(tmp_path / "nope.csv")
    other = tmp_path / "bars.xlsx"
    other.write_text("x")
    with pytest.raises(DataError, match="unsupported"):
        load_dataset(other)
    corrupt = tmp_path / "bars.parquet"
    corrupt.write_bytes(b"this is not a parquet file")
    with pytest.raises(DataError, match="cannot read Parquet"):
        load_dataset(corrupt)
    with pytest.raises(DataError, match="no market data"):
        load_dataset()


def test_bad_corporate_action_rows_are_refused(files, demo):
    tmp, bars = files
    actions = write_corporate_actions(demo, tmp / "actions.csv")
    actions.write_text(actions.read_text() + "2023-06-01T00:00:00Z,AAA,merger,1\n")
    with pytest.raises(DataError, match="actions.csv row"):
        load_dataset(bars, corporate_actions=actions)


# -- rejected writes --------------------------------------------------------------------


@pytest.fixture
def result(demo):
    return run_backtest(demo, Momentum(lookback=10), BacktestConfig())


@pytest.mark.skipif(os.geteuid() == 0, reason="root ignores directory permissions")
def test_an_unwritable_destination_fails_cleanly_and_leaves_nothing_behind(files, capsys):
    tmp, bars = files
    locked = tmp / "locked"
    locked.mkdir()
    locked.chmod(0o555)
    try:
        code = main(
            ["run", "--strategy", MOMENTUM, "--data", str(bars), "--out", str(locked / "run")]
        )
        assert code == EXIT_FAILED
        assert "Permission denied" in capsys.readouterr().err
        assert list(locked.iterdir()) == []
    finally:
        locked.chmod(0o755)


def test_a_failure_part_way_through_writing_leaves_no_partial_directory(
    tmp_path, result, monkeypatch
):
    real_write = Path.write_text
    written = []

    def failing_write(self, text, *args, **kwargs):
        written.append(self.name)
        if len(written) == 4:
            raise OSError(28, "No space left on device")
        return real_write(self, text, *args, **kwargs)

    monkeypatch.setattr(Path, "write_text", failing_write)
    with pytest.raises(OSError, match="No space left"):
        write_artifacts(result, tmp_path / "run")
    assert list(tmp_path.iterdir()) == []


def test_a_failed_rewrite_keeps_the_previous_run_intact(tmp_path, result, monkeypatch):
    out = write_artifacts(result, tmp_path / "run")
    before = {p.name: p.read_bytes() for p in out.iterdir()}

    def no_rename(src, dst):
        raise OSError(5, "Input/output error")

    monkeypatch.setattr(os, "rename", no_rename)
    with pytest.raises(OSError):
        write_artifacts(result, tmp_path / "run2")
    monkeypatch.undo()

    assert {p.name: p.read_bytes() for p in out.iterdir()} == before
    assert sorted(p.name for p in tmp_path.iterdir()) == ["run"]


def test_rewriting_a_run_replaces_it_completely(tmp_path, result):
    out = write_artifacts(result, tmp_path / "run")
    (out / "stale.txt").write_text("left over from an older run")
    again = write_artifacts(result, tmp_path / "run")
    assert again == out and not (out / "stale.txt").exists()
    assert len(list(out.iterdir())) == 8
