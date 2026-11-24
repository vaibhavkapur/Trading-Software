"""Regenerate the golden replay fixture under tests/fixtures/golden.

    python scripts/update_golden.py              # recompute the expected values
    python scripts/update_golden.py --new-data   # also regenerate the synthetic input files

The replay test (tests/integration/test_determinism.py) runs each case in cases.json over
the committed data and compares against the expected values stored there. They should
change only when the fill model, the accounting or an example strategy changes on
purpose. When that happens: bump FILL_MODEL_VERSION if fills changed, run this script,
and review the diff of cases.json before committing it.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
GOLDEN = ROOT / "tests" / "fixtures" / "golden"
sys.path.insert(0, str(ROOT / "src"))

from backtester import BacktestConfig, fmt_units, load_dataset, run_backtest  # noqa: E402
from backtester.fixtures import demo_dataset, write_bars, write_corporate_actions  # noqa: E402
from backtester.strategy import load_strategy_class  # noqa: E402


def load_data():
    return load_dataset(GOLDEN / "bars.csv", corporate_actions=GOLDEN / "corporate_actions.csv")


def run_case(case: dict, dataset):
    cls = load_strategy_class(ROOT / case["strategy"])
    return run_backtest(dataset, cls(**case["params"]), BacktestConfig.from_dict(case["config"]))


def observed(result) -> dict:
    """What the replay test pins. Integers and hashes only, so it is platform-independent."""
    decimals = result.config.price_decimals
    return {
        "result_hash": result.result_hash,
        "orders": len(result.orders),
        "fills": len(result.fills),
        "ledger_entries": len(result.ledger),
        "final_equity": fmt_units(result.metrics.final_equity, decimals),
        "fees": fmt_units(result.metrics.fees, decimals),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--new-data", action="store_true", help="regenerate the input files too")
    args = parser.parse_args()

    document = json.loads((GOLDEN / "cases.json").read_text())
    if args.new_data:
        source = document["data"]["generator"]
        synthetic = demo_dataset(seed=source["seed"], days=source["days"])
        write_bars(synthetic, GOLDEN / "bars.csv")
        write_corporate_actions(synthetic, GOLDEN / "corporate_actions.csv")

    dataset = load_data()
    changed = document["data"].get("data_version") != dataset.data_version
    document["data"]["data_version"] = dataset.data_version
    for case in document["cases"]:
        now = observed(run_case(case, dataset))
        if case.get("expected") != now:
            changed = True
            print(f"{case['name']}: expected values changed")
            for key, value in now.items():
                before = (case.get("expected") or {}).get(key)
                if before != value:
                    print(f"  {key}: {before} -> {value}")
        case["expected"] = now
    (GOLDEN / "cases.json").write_text(json.dumps(document, indent=2) + "\n")
    print("cases.json updated; review the diff" if changed else "nothing changed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
