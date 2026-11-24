"""How the engine behaves when its inputs or outputs misbehave: ``make failure-demo``.

Each case below is a failure a backtest can meet in practice. The point of the demo is
that none of them silently produces a plausible-looking wrong answer. All data is
synthetic.
"""

from __future__ import annotations

import os
import random
import tempfile
from pathlib import Path

from backtester import (
    BacktestConfig,
    DataError,
    Dataset,
    EventType,
    RiskLimits,
    Strategy,
    fmt_ts,
    fmt_units,
    load_dataset,
    run_backtest,
)
from backtester.fixtures import demo_dataset, write_bars
from backtester.report import write_artifacts
from backtester.timeutil import NS_PER_DAY


class BuyOnAnotherSymbolsBar(Strategy):
    """Buys 10 CCC every time an AAA bar arrives, whether or not CCC has fresh data."""

    def on_market_event(self, event, context):
        if event.event_type is EventType.BAR and event.symbol == "AAA":
            context.submit_order("CCC", "buy", 10)


def heading(text: str) -> None:
    print()
    print(text)
    print("-" * len(text))


def stale_data(dataset: Dataset) -> None:
    heading("1. A feed goes quiet: CCC publishes nothing for four weekdays")
    gap = dataset.summary.symbols[2].gaps[0]
    print(f"   last bar before the hole opens {fmt_ts(gap.after)}, next one {fmt_ts(gap.before)}")
    limit_days = 3  # long enough to span an ordinary weekend
    unguarded = run_backtest(dataset, BuyOnAnotherSymbolsBar(), BacktestConfig())
    guarded = run_backtest(
        dataset,
        BuyOnAnotherSymbolsBar(),
        BacktestConfig(risk=RiskLimits(max_data_age_ns=limit_days * NS_PER_DAY)),
    )

    def outcome(order) -> str:
        if order.filled_qty:
            return f"filled {fmt_ts(order.closed_at)[:10]} at {fmt_units(order.avg_fill_px, 4)}"
        return f"{order.status.value} {order.reason}"

    print()
    limit_column = f"max_data_age = {limit_days} days"
    print(f"   {'order sent':<12}{'decided at':>11}   {'no staleness limit':<32}{limit_column}")
    for free, limited in zip(unguarded.orders, guarded.orders, strict=True):
        if gap.after + NS_PER_DAY < free.submitted_at <= gap.before + NS_PER_DAY:
            print(
                f"   {fmt_ts(free.submitted_at)[:10]:<12}{fmt_units(free.decision_px, 4):>11}   "
                f"{outcome(free):<32}{outcome(limited)}"
            )
    print()
    print("   Without a limit, orders are priced off a stale mark and fill days later at a")
    print("   different price. With one, they are refused. Monday's order passes because three")
    print("   days of silence looks the same as a weekend; the gap itself is always listed in")
    print("   the report's Data section.")


def clock_skew(dataset: Dataset, work: Path) -> None:
    heading("2. A skewed clock: one bar claims to be published before it closed")
    path = write_bars(dataset, work / "skewed.csv")
    lines = path.read_text().splitlines()
    fields = lines[100].split(",")
    fields[3] = fields[1]  # ts_avail := ts_open, which is before ts_close
    lines[100] = ",".join(fields)
    path.write_text("\n".join(lines) + "\n")
    try:
        load_dataset(path)
    except DataError as exc:
        print(f"   refused: {exc}")
    dropped = load_dataset(path, on_invalid="drop")
    print(
        f"   with --on-invalid drop: loaded {len(dropped)} bars, "
        f"{dropped.invalid_dropped} invalid row dropped and counted in the report."
    )


def duplicate_delivery(dataset: Dataset, work: Path) -> None:
    heading("3. Duplicate and out-of-order delivery: every 10th row repeated, rows shuffled")
    clean = write_bars(dataset, work / "clean.csv")
    header, *rows = clean.read_text().splitlines()
    rows += rows[::10]
    random.Random(1).shuffle(rows)
    messy = work / "messy.csv"
    messy.write_text("\n".join([header, *rows]) + "\n")
    loaded = load_dataset(messy)
    print(
        f"   rows in file {len(rows)}, bars kept {len(loaded)}, "
        f"duplicates dropped {loaded.duplicates_dropped}"
    )
    same = loaded.data_version == load_dataset(clean).data_version
    print(f"   data version identical to the clean file: {same}")


def conflicting_revision(dataset: Dataset, work: Path) -> None:
    heading("4. A revised bar: the same bar appears twice with different closes")
    path = write_bars(dataset, work / "revised.csv")
    lines = path.read_text().splitlines()
    fields = lines[50].split(",")
    # Stay inside the bar's high/low so the row is valid on its own, just different.
    fields[7] = fields[4] if fields[7] != fields[4] else fields[5]
    lines.append(",".join(fields))
    path.write_text("\n".join(lines) + "\n")
    try:
        load_dataset(path)
    except DataError as exc:
        print(f"   refused: {exc}")


def rejected_write(dataset: Dataset, work: Path) -> None:
    heading("5. A rejected write: the output directory cannot be written")
    result = run_backtest(dataset, BuyOnAnotherSymbolsBar(), BacktestConfig())
    locked = work / "locked"
    locked.mkdir()
    os.chmod(locked, 0o555)
    try:
        write_artifacts(result, locked / "run")
        print("   unexpectedly succeeded (running as a user that ignores permissions?)")
    except OSError as exc:
        print(f"   failed cleanly: {type(exc).__name__}: {exc.strerror}")
        print(f"   files left behind in the target: {sorted(p.name for p in locked.iterdir())}")
    finally:
        os.chmod(locked, 0o755)
    out = write_artifacts(result, work / "ok" / "run")
    print(f"   the same result written elsewhere: {len(list(out.iterdir()))} files, all or nothing")


def main() -> None:
    print("FAILURE DEMO. Synthetic data, simulated orders only.")
    dataset = demo_dataset()
    with tempfile.TemporaryDirectory() as tmp:
        work = Path(tmp)
        stale_data(dataset)
        clock_skew(dataset, work)
        duplicate_delivery(dataset, work)
        conflicting_revision(dataset, work)
        rejected_write(dataset, work)
    print()


if __name__ == "__main__":
    main()
