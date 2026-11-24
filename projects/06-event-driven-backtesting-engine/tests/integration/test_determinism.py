"""The same dataset, config and seed produce identical trades and reports."""

import json
import os
import subprocess
import sys
from dataclasses import replace
from pathlib import Path

import pytest
from mean_reversion import MeanReversion
from momentum import Momentum

from backtester import BacktestConfig, ExecutionConfig, RiskLimits, load_dataset, run_backtest
from backtester.fixtures import demo_dataset, write_bars, write_corporate_actions
from backtester.report import deterministic_files, write_artifacts
from backtester.strategy import load_strategy_class
from backtester.timeutil import NS_PER_MS

ROOT = Path(__file__).resolve().parents[2]
GOLDEN = ROOT / "tests" / "fixtures" / "golden"
CASES = json.loads((GOLDEN / "cases.json").read_text())

STOCHASTIC = BacktestConfig(
    seed=42,
    execution=ExecutionConfig(
        fee_bps=1,
        slippage_bps=3,
        spread_bps=2,
        latency_ns=40 * NS_PER_MS,
        latency_jitter_ns=30 * NS_PER_MS,
        participation_rate="0.0005",
        touch_fill_probability="0.5",
    ),
    risk=RiskLimits(allow_short=True),
)


@pytest.fixture(scope="module")
def data():
    return demo_dataset(seed=7, days=330)


@pytest.fixture(scope="module")
def golden_data():
    return load_dataset(GOLDEN / "bars.csv", corporate_actions=GOLDEN / "corporate_actions.csv")


# -- replay against committed expectations ----------------------------------------------


def test_the_golden_input_files_are_the_ones_the_expectations_were_made_from(golden_data):
    assert golden_data.data_version == CASES["data"]["data_version"]
    generator = CASES["data"]["generator"]
    regenerated = demo_dataset(seed=generator["seed"], days=generator["days"])
    assert regenerated.data_version == golden_data.data_version  # the generator is stable too


@pytest.mark.parametrize("case", CASES["cases"], ids=lambda case: case["strategy"])
def test_golden_replay(golden_data, case):
    cls = load_strategy_class(ROOT / case["strategy"])
    config = BacktestConfig.from_dict(case["config"])
    result = run_backtest(golden_data, cls(**case["params"]), config)
    expected = case["expected"]

    decimals = config.price_decimals
    assert len(result.orders) == expected["orders"]
    assert len(result.fills) == expected["fills"]
    assert len(result.ledger) == expected["ledger_entries"]
    assert result.metrics.final_equity == round(float(expected["final_equity"]) * 10**decimals)
    assert result.metrics.fees == round(float(expected["fees"]) * 10**decimals)
    assert result.result_hash == expected["result_hash"], (
        "The trades changed. If that was intended, bump FILL_MODEL_VERSION where fills "
        "changed and run scripts/update_golden.py."
    )


# -- repeat runs ------------------------------------------------------------------------


@pytest.mark.parametrize(
    "make", [lambda: Momentum(lookback=15), lambda: MeanReversion(window=10, entry_bps=150)]
)
def test_repeat_runs_are_identical_in_every_artifact(data, make):
    first = run_backtest(data, make(), STOCHASTIC)
    second = run_backtest(data, make(), STOCHASTIC)
    assert first.fills and first.fills == second.fills
    assert first.orders == second.orders and first.ledger == second.ledger
    assert first.snapshots == second.snapshots and first.metrics == second.metrics
    assert (first.run, first.result_hash) == (second.run, second.result_hash)
    assert deterministic_files(first) == deterministic_files(second)


def test_runtime_measurements_are_the_only_thing_that_differs_on_disk(data, tmp_path):
    for name in ("a", "b"):
        write_artifacts(run_backtest(data, Momentum(lookback=15), STOCHASTIC), tmp_path / name)
    names = sorted(p.name for p in (tmp_path / "a").iterdir())
    assert names == sorted(
        ["report.md", "metrics.json", "orders.csv", "fills.csv", "ledger.csv", "equity.csv"]
        + ["manifest.json", "runtime.json"]
    )
    for name in names:
        if name != "runtime.json":
            assert (tmp_path / "a" / name).read_bytes() == (tmp_path / "b" / name).read_bytes()
    runtime = json.loads((tmp_path / "a" / "runtime.json").read_text())
    assert 0 < runtime["event_loop_seconds"] <= runtime["total_seconds"]
    for name in ("report.md", "metrics.json", "manifest.json"):
        assert "seconds" not in (tmp_path / "a" / name).read_text()


def test_separate_processes_with_different_hash_seeds_write_identical_runs(data, tmp_path):
    """Set and dict ordering, and anything else process-specific, must not leak in."""
    bars = write_bars(data, tmp_path / "bars.csv")
    actions = write_corporate_actions(data, tmp_path / "actions.csv")
    command = [
        sys.executable, "-m", "backtester", "run",
        "--strategy", str(ROOT / "examples" / "mean_reversion.py"),
        "--param", "window=10", "--param", "entry_bps=150",
        "--data", str(bars), "--corporate-actions", str(actions),
        "--seed", "42", "--fee-bps", "1", "--slippage-bps", "3", "--latency-ms", "40",
        "--latency-jitter-ms", "30", "--participation", "0.0005", "--touch-fill-prob", "0.5",
    ]  # fmt: skip
    for hash_seed in ("1", "2"):
        env = {**os.environ, "PYTHONPATH": str(ROOT / "src"), "PYTHONHASHSEED": hash_seed}
        done = subprocess.run(
            [*command, "--out", str(tmp_path / hash_seed)], env=env, capture_output=True, text=True
        )
        assert done.returncode == 0, done.stderr
        assert "SIMULATED RESULTS" in done.stdout

    files = sorted(p.name for p in (tmp_path / "1").iterdir())
    assert len(files) == 8
    for name in files:
        if name != "runtime.json":
            assert (tmp_path / "1" / name).read_bytes() == (tmp_path / "2" / name).read_bytes()
    assert len((tmp_path / "1" / "fills.csv").read_text().splitlines()) > 50


# -- what the hashes do and do not depend on --------------------------------------------


def test_the_seed_matters_only_when_something_is_random(data):
    plain = BacktestConfig(execution=ExecutionConfig(fee_bps=1, slippage_bps=3))
    a = run_backtest(data, Momentum(lookback=15), replace(plain, seed=1))
    b = run_backtest(data, Momentum(lookback=15), replace(plain, seed=2))
    assert a.result_hash == b.result_hash  # nothing draws from the seed
    assert a.run.id != b.run.id  # but the seed is still part of the run's identity

    c = run_backtest(data, MeanReversion(window=10, entry_bps=150), replace(STOCHASTIC, seed=1))
    d = run_backtest(data, MeanReversion(window=10, entry_bps=150), replace(STOCHASTIC, seed=2))
    assert c.result_hash != d.result_hash


def test_every_assumption_that_changes_the_trades_changes_the_config_hash(data):
    base = BacktestConfig(execution=ExecutionConfig(fee_bps=1, slippage_bps=3, spread_bps=2))
    reference = run_backtest(data, Momentum(lookback=15), base)
    variants = {
        "fee": replace(base, execution=replace(base.execution, fee_bps=2)),
        "slippage": replace(base, execution=replace(base.execution, slippage_bps=4)),
        "spread": replace(base, execution=replace(base.execution, spread_bps=3)),
        "cash": replace(base, initial_cash=base.initial_cash // 2),
    }
    hashes = {reference.run.config_hash}
    for name, config in variants.items():
        result = run_backtest(data, Momentum(lookback=15), config)
        assert result.result_hash != reference.result_hash, name
        hashes.add(result.run.config_hash)
    assert len(hashes) == len(variants) + 1

    other_params = run_backtest(data, Momentum(lookback=16), base)
    assert other_params.run.config_hash != reference.run.config_hash


def test_equal_configs_spelled_differently_hash_the_same(data):
    a = BacktestConfig(execution=ExecutionConfig(fee_bps="1", slippage_bps=3))
    b = BacktestConfig(execution=ExecutionConfig(fee_bps="1.0", slippage_bps="3.00"))
    first = run_backtest(data, Momentum(lookback=15), a)
    second = run_backtest(data, Momentum(lookback=15), b)
    assert first.run == second.run and first.result_hash == second.result_hash
    assert deterministic_files(first) == deterministic_files(second)


def test_the_data_version_follows_content_not_files(data, tmp_path):
    csv = load_dataset(write_bars(data, tmp_path / "bars.csv"))
    parquet = load_dataset(write_bars(data, tmp_path / "bars.parquet"))
    assert csv.data_version == parquet.data_version
    assert csv.events == parquet.events

    lines = (tmp_path / "bars.csv").read_text().splitlines()
    edited = lines[40].split(",")
    edited[-1] = str(int(edited[-1]) + 1)  # one more unit of volume on one bar
    (tmp_path / "bars.csv").write_text("\n".join([*lines[:40], ",".join(edited), *lines[41:]]))
    assert load_dataset(tmp_path / "bars.csv").data_version != csv.data_version
