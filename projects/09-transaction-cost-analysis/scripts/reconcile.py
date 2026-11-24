"""Check a report bundle against its raw inputs without using the tca package.

Usage: python scripts/reconcile.py --data examples/data --report examples/report

Two checks, standard library only:

1. Every file listed in ``manifest.json`` still has the recorded SHA-256.
2. Arrival-price shortfall is recomputed for each order from ``orders.csv``, ``fills.csv``
   and ``quotes.csv`` using exact fractions, and compared with the report's ``orders.csv``
   to within half a unit of the last exported decimal place.

The recomputation follows the documented defaults (event-time quotes, five-second staleness
limit). Orders with no fresh quote at arrival are not recomputed; the check there is that
the report does not call them trusted.

Exit status 0 when everything reconciles, 1 when anything differs, 2 on usage errors.
"""

from __future__ import annotations

import argparse
import csv
import hashlib
import json
import sys
from collections import defaultdict
from datetime import UTC, datetime, timedelta
from fractions import Fraction
from pathlib import Path

EPOCH = datetime(1970, 1, 1, tzinfo=UTC)
TOLERANCE = Fraction(1, 20_000)  # half of 0.0001


def micros(text: str) -> int:
    return (datetime.fromisoformat(text) - EPOCH) // timedelta(microseconds=1)


def rows(path: Path) -> list[dict[str, str]]:
    with path.open(newline="", encoding="utf-8") as handle:
        return list(csv.DictReader(handle))


def check_hashes(report: Path, manifest: dict) -> list[str]:
    problems = []
    for name, info in manifest["files"].items():
        path = report / name
        if not path.exists():
            problems.append(f"{name}: listed in the manifest but missing")
            continue
        digest = f"sha256:{hashlib.sha256(path.read_bytes()).hexdigest()}"
        if digest != info["sha256"]:
            problems.append(f"{name}: content does not match the manifest hash")
    return problems


def arrival_mid(quotes: list[tuple], arrival: int, max_age_us: int) -> Fraction | None:
    """Mid of the best bid and offer across venues whose latest quote at or before
    ``arrival`` is no older than ``max_age_us``. None when no venue qualifies."""
    latest: dict[str, tuple] = {}
    for ts, venue, bid, ask in quotes:
        if ts <= arrival and (venue not in latest or ts > latest[venue][0]):
            latest[venue] = (ts, bid, ask)
    fresh = [quote for quote in latest.values() if arrival - quote[0] <= max_age_us]
    if not fresh:
        return None
    return (max(quote[1] for quote in fresh) + min(quote[2] for quote in fresh)) / 2


def check_orders(data: Path, report: Path, max_age_us: int) -> tuple[int, int, list[str]]:
    quotes = defaultdict(list)
    for row in rows(data / "quotes.csv"):
        quotes[row["symbol"]].append(
            (micros(row["ts"]), row["venue"], Fraction(row["bid"]), Fraction(row["ask"]))
        )
    fills = defaultdict(list)
    for row in rows(data / "fills.csv"):
        fills[row["order_id"]].append((int(row["qty"]), Fraction(row["price"])))
    reported = {row["order_id"]: row for row in rows(report / "orders.csv")}

    orders = rows(data / "orders.csv")
    known = {order["order_id"] for order in orders}
    problems = [
        f"{order_id}: in the report but not in the inputs" for order_id in reported.keys() - known
    ]
    reconciled = skipped = 0
    for order in orders:
        order_id = order["order_id"]
        row = reported.get(order_id)
        if row is None:
            problems.append(f"{order_id}: in the inputs but not in the report")
            continue
        mid = arrival_mid(quotes[order["symbol"]], micros(order["arrival_ts"]), max_age_us)
        filled = sum(qty for qty, _ in fills[order_id])
        if mid is None:
            skipped += 1
            if row["arrival_mid_trusted"] == "true":
                problems.append(f"{order_id}: no fresh arrival quote, yet reported as trusted")
            continue
        if Fraction(row["arrival_mid"]) != mid:
            problems.append(f"{order_id}: arrival mid {row['arrival_mid']}, expected {float(mid)}")
            continue
        if not filled:
            skipped += 1
            if row["arrival_mid_cost"]:
                problems.append(f"{order_id}: has no fills, yet a cost is reported")
            continue
        sign = 1 if order["side"] == "buy" else -1
        cost = sign * sum(qty * (price - mid) for qty, price in fills[order_id])
        bps = cost / (filled * mid) * 10_000
        for column, expected in (("arrival_mid_cost", cost), ("arrival_mid_bps", bps)):
            if abs(Fraction(row[column]) - expected) > TOLERANCE:
                problems.append(f"{order_id}: {column} {row[column]}, expected {float(expected)}")
        reconciled += 1
    return reconciled, skipped, problems


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--data", required=True, type=Path, help="directory of input CSVs")
    parser.add_argument("--report", required=True, type=Path, help="report bundle directory")
    args = parser.parse_args(argv)

    manifest_path = args.report / "manifest.json"
    if not manifest_path.exists():
        print(f"error: {manifest_path} not found", file=sys.stderr)
        return 2
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    parameters = manifest["methodology"]["parameters"]
    if parameters["quote_clock"] != "event":
        print("error: this script only recomputes event-clock reports", file=sys.stderr)
        return 2

    problems = check_hashes(args.report, manifest)
    reconciled, skipped, order_problems = check_orders(
        args.data, args.report, parameters["max_quote_age_ms"] * 1000
    )
    problems += order_problems
    print(f"report {manifest['report_id']}  methodology {manifest['methodology']['id']}")
    print(f"dataset {manifest['dataset']['version']}")
    print(
        f"{len(manifest['files'])} files hashed, {reconciled} orders recomputed, {skipped} skipped"
    )
    for problem in problems:
        print(f"MISMATCH {problem}")
    print("OK" if not problems else f"FAILED: {len(problems)} problem(s)")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
