"""Terminal walkthrough on synthetic data: ``make demo`` or ``python -m backtester.demo``.

Every step is the real command line, run in-process, so what is printed is what you
would get by typing the command shown.
"""

from __future__ import annotations

import shlex
import sys
from pathlib import Path

from .cli import EXIT_OK, main
from .report import BANNER

DATA = "data/demo"
RUN = "runs/demo-momentum"
RERUN = "runs/demo-momentum-rerun"
WALK = "runs/demo-walkforward"
FRICTION = [
    "--corporate-actions",
    f"{DATA}/corporate_actions.csv",
    "--fee-bps",
    "1",
    "--slippage-bps",
    "3",
    "--spread-bps",
    "2",
    "--latency-ms",
    "50",
    "--participation",
    "0.05",
    "--seed",
    "42",
]


def _step(title: str, argv: list[str]) -> None:
    print()
    print("=" * 88)
    print(title)
    print("=" * 88)
    print("$ python -m backtester " + " ".join(shlex.quote(a) for a in argv))
    print()
    code = main(argv)
    if code != EXIT_OK:
        raise SystemExit(f"demo step failed with exit code {code}")


def run_demo() -> int:
    strategy = Path("examples/momentum.py")
    if not strategy.is_file():
        print(
            "run the demo from the project root (examples/momentum.py not found)", file=sys.stderr
        )
        return 2

    print("EVENT-DRIVEN BACKTESTING ENGINE: DEMO")
    print(BANNER)
    print("All prices below are invented by a seeded random walk. They are not market data.")

    _step(
        "1. Generate a synthetic dataset (3 symbols, a split, a dividend, a data gap)",
        ["gen-data", "--out", DATA, "--seed", "7"],
    )

    _step(
        "2. Backtest a momentum strategy with fees, spread, slippage, latency and partial fills",
        [
            "run",
            "--strategy",
            str(strategy),
            "--data",
            f"{DATA}/bars.parquet",
            *FRICTION,
            "--param",
            "lookback=40",
            "--oos-start",
            "2024-01-01",
            "--out",
            RUN,
        ],
    )

    _step(
        "3. Deterministic replay: re-run from the saved manifest and compare every artifact",
        ["verify", RUN],
    )

    _step(
        "4. Same run from the CSV copy of the data: the data version and result hash match step 2",
        [
            "run",
            "--strategy",
            str(strategy),
            "--data",
            f"{DATA}/bars.csv",
            *FRICTION,
            "--param",
            "lookback=40",
            "--oos-start",
            "2024-01-01",
            "--out",
            RERUN,
        ],
    )

    _step(
        "5. Walk-forward: choose parameters on each training window, evaluate on unseen data",
        [
            "walk-forward",
            "--strategy",
            str(strategy),
            "--data",
            f"{DATA}/bars.parquet",
            *FRICTION,
            "--grid",
            "lookback=10,20,40,80",
            "--grid",
            "threshold_bps=0,200",
            "--train-days",
            "365",
            "--test-days",
            "91",
            "--out",
            WALK,
        ],
    )

    print()
    print("=" * 88)
    print("Done. Reports to read:")
    print(f"  {RUN}/report.md            assumptions, data gaps, in/out-of-sample, reconciliation")
    print(f"  {WALK}/walkforward.md   per-fold selection and the chained out-of-sample result")
    print(
        "Next: `make failure-demo` shows stale data, skewed clocks, duplicates and failed writes."
    )
    print("=" * 88)
    return EXIT_OK


if __name__ == "__main__":
    sys.exit(run_demo())
