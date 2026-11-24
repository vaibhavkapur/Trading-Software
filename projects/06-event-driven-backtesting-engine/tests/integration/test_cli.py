"""The command line from end to end: generate data, run, write artifacts, verify."""

import csv
import json
from pathlib import Path

import pytest
from helpers import SESSION_OPEN, T0

from backtester.cli import EXIT_FAILED, EXIT_OK, EXIT_USAGE, main
from backtester.timeutil import NS_PER_SEC, fmt_ts

ROOT = Path(__file__).resolve().parents[2]
MOMENTUM = str(ROOT / "examples" / "momentum.py")
MEAN_REVERSION = str(ROOT / "examples" / "mean_reversion.py")


@pytest.fixture(scope="module")
def demo(tmp_path_factory):
    out = tmp_path_factory.mktemp("data")
    assert main(["gen-data", "--out", str(out), "--days", "330"]) == EXIT_OK
    return out


def run_args(demo: Path, out: Path, *extra: str, data: str = "bars.parquet") -> list[str]:
    return [
        "run", "--strategy", MOMENTUM, "--param", "lookback=20",
        "--data", str(demo / data), "--corporate-actions", str(demo / "corporate_actions.csv"),
        "--slippage-bps", "3", "--fee-bps", "1", "--seed", "42", "--out", str(out), *extra,
    ]  # fmt: skip


def read_csv(path: Path) -> list[dict[str, str]]:
    with path.open(newline="") as handle:
        return list(csv.DictReader(handle))


# -- gen-data ---------------------------------------------------------------------------


def test_gen_data_writes_csv_parquet_and_corporate_actions(demo, capsys):
    assert sorted(p.name for p in demo.iterdir()) == [
        "bars.csv",
        "bars.parquet",
        "corporate_actions.csv",
    ]
    actions = read_csv(demo / "corporate_actions.csv")
    assert sorted((row["symbol"], row["type"]) for row in actions) == [
        ("AAA", "dividend"),
        ("BBB", "split"),
    ]
    assert len(read_csv(demo / "bars.csv")) == 3 * 330 - 4  # CCC is missing four sessions


def test_gen_data_says_the_prices_are_invented(tmp_path, capsys):
    assert main(["gen-data", "--out", str(tmp_path), "--symbols", "X,Y", "--days", "5"]) == EXIT_OK
    out = capsys.readouterr().out
    assert "invented prices" in out and "not market data" in out
    assert not (tmp_path / "corporate_actions.csv").exists()
    assert {row["symbol"] for row in read_csv(tmp_path / "bars.csv")} == {"X", "Y"}


# -- run --------------------------------------------------------------------------------


def test_run_writes_a_complete_self_describing_run_directory(demo, tmp_path, capsys):
    out = tmp_path / "run"
    assert main(run_args(demo, out, "--oos-start", "2024-01-01")) == EXIT_OK
    printed = capsys.readouterr().out
    assert printed.startswith("SIMULATED RESULTS")
    assert "cash and positions match the ledger and fills" in printed

    manifest = json.loads((out / "manifest.json").read_text())
    assert manifest["simulated"] is True and manifest["seed"] == 42
    assert manifest["config"]["execution"]["slippage_bps"] == "3"
    assert manifest["strategy"]["name"] == "Momentum"
    assert manifest["strategy"]["params"] == {"lookback": 20}
    assert len(manifest["strategy"]["source_sha256"]) == 64
    assert manifest["run_id"] in printed and manifest["result_hash"] in printed
    assert [a["action"] for a in manifest["data"]["corporate_actions"]] == [
        "dividend 0.5000 per unit",
        "split 2-for-1",
    ]

    fills = read_csv(out / "fills.csv")
    orders = read_csv(out / "orders.csv")
    ledger = read_csv(out / "ledger.csv")
    equity = read_csv(out / "equity.csv")
    assert len(fills) > 20 and len(orders) >= len(fills)
    assert {row["order_id"] for row in fills} <= {row["id"] for row in orders}
    assert ledger[0]["kind"] == "deposit" and ledger[0]["amount"] == "1000000.0000"
    assert {"trade", "fee", "dividend", "split"} <= {row["kind"] for row in ledger}
    assert equity[-1]["cash"] == ledger[-1]["cash_after"]

    metrics = json.loads((out / "metrics.json").read_text())
    assert metrics["whole_run"]["final_equity"] == equity[-1]["equity"]
    assert metrics["in_sample"] is not None and metrics["out_of_sample"] is not None
    assert metrics["whole_run"]["fill_count"] == len(fills)

    report = (out / "report.md").read_text()
    assert "In-sample is everything before 2024-01-01T00:00:00Z" in report


def test_the_blueprint_command_line_runs_as_written(demo, tmp_path, monkeypatch, capsys):
    monkeypatch.chdir(tmp_path)
    (tmp_path / "examples").mkdir()
    (tmp_path / "examples" / "momentum.py").write_text(Path(MOMENTUM).read_text())
    (tmp_path / "data").mkdir()
    (tmp_path / "data" / "xyz_2024.parquet").write_bytes((demo / "bars.parquet").read_bytes())
    code = main(
        ["run", "--strategy", "examples/momentum.py", "--data", "data/xyz_2024.parquet",
         "--slippage-bps", "3", "--fee-bps", "1", "--seed", "42"]
    )  # fmt: skip
    assert code == EXIT_OK
    (run_dir,) = (tmp_path / "runs").iterdir()
    assert run_dir.name == json.loads((run_dir / "manifest.json").read_text())["run_id"]
    capsys.readouterr()
    assert main(["verify", str(run_dir)]) == EXIT_OK


def test_csv_and_parquet_inputs_give_the_same_run(demo, tmp_path):
    assert main(run_args(demo, tmp_path / "parquet")) == EXIT_OK
    assert main(run_args(demo, tmp_path / "csv", data="bars.csv")) == EXIT_OK
    a = json.loads((tmp_path / "parquet" / "manifest.json").read_text())
    b = json.loads((tmp_path / "csv" / "manifest.json").read_text())
    for key in ("run_id", "data_version", "config_hash", "result_hash"):
        assert a[key] == b[key]
    for name in ("fills.csv", "orders.csv", "ledger.csv", "equity.csv", "metrics.json"):
        assert (tmp_path / "parquet" / name).read_bytes() == (tmp_path / "csv" / name).read_bytes()


def test_no_artifacts_prints_the_summary_and_writes_nothing(demo, tmp_path, monkeypatch, capsys):
    monkeypatch.chdir(tmp_path)
    assert main(run_args(demo, tmp_path / "run", "--no-artifacts")) == EXIT_OK
    assert "result hash" in capsys.readouterr().out
    assert list(tmp_path.iterdir()) == []


def test_run_on_quotes(tmp_path, capsys):
    quotes = tmp_path / "quotes.csv"
    rows = ["symbol,ts_event,bid,ask,bid_size,ask_size"]
    for i in range(600):
        ts = T0 + SESSION_OPEN + i * 60 * NS_PER_SEC
        mid = 10_000 + (i * 37) % 400 - (i * 11) % 150  # cents; wanders without any randomness
        rows.append(f"Q,{fmt_ts(ts)},{(mid - 2) / 100:.2f},{(mid + 2) / 100:.2f},500,500")
    quotes.write_text("\n".join(rows) + "\n")
    out = tmp_path / "run"
    code = main(
        ["run", "--strategy", MEAN_REVERSION, "--quotes", str(quotes), "--calendar", "24x7",
         "--latency-ms", "5", "--max-quote-age-ms", "60000", "--out", str(out)]
    )  # fmt: skip
    assert code == EXIT_OK
    report = (out / "report.md").read_text()
    assert "| Q | quote | 600 |" in report and "Quote fills:" in report
    assert "Bar fills:" not in report
    capsys.readouterr()
    assert main(["verify", str(out)]) == EXIT_OK


# -- verify -----------------------------------------------------------------------------


@pytest.fixture
def saved_run(demo, tmp_path, capsys):
    """A run made from copies of the inputs, so tests can damage them."""
    work = tmp_path / "inputs"
    work.mkdir()
    for name in ("bars.csv", "corporate_actions.csv"):
        (work / name).write_bytes((demo / name).read_bytes())
    strategy = work / "momentum.py"
    strategy.write_text(Path(MOMENTUM).read_text())
    out = tmp_path / "run"
    args = run_args(work, out, data="bars.csv")
    args[args.index("--strategy") + 1] = str(strategy)
    assert main(args) == EXIT_OK
    capsys.readouterr()
    return work, out


def test_verify_reproduces_an_untouched_run(saved_run, capsys):
    _, out = saved_run
    assert main(["verify", str(out)]) == EXIT_OK
    printed = capsys.readouterr().out
    assert "REPRODUCED" in printed and "FAIL" not in printed
    assert printed.count("  ok  ") == 9  # three hashes and six artifacts


@pytest.mark.parametrize("artifact", ["fills.csv", "report.md", "metrics.json", "equity.csv"])
def test_verify_catches_an_edited_artifact(saved_run, artifact, capsys):
    _, out = saved_run
    path = out / artifact
    path.write_text(path.read_text().replace("1", "7", 1))
    assert main(["verify", str(out)]) == EXIT_FAILED
    printed = capsys.readouterr().out
    assert f"FAIL  {artifact} (file on disk)" in printed and "NOT REPRODUCED: 1 of" in printed


def test_verify_catches_a_deleted_artifact(saved_run, capsys):
    _, out = saved_run
    (out / "ledger.csv").unlink()
    assert main(["verify", str(out)]) == EXIT_FAILED
    assert "missing" in capsys.readouterr().out


def test_verify_catches_changed_input_data(saved_run, capsys):
    work, out = saved_run
    bars = work / "bars.csv"
    lines = bars.read_text().splitlines()
    bars.write_text("\n".join(lines[:-1]) + "\n")  # the last bar has gone
    assert main(["verify", str(out)]) == EXIT_FAILED
    printed = capsys.readouterr().out
    assert "FAIL  data version" in printed and "FAIL  result hash" in printed


def test_verify_catches_a_changed_strategy(saved_run, capsys):
    work, out = saved_run
    strategy = work / "momentum.py"
    strategy.write_text(strategy.read_text() + "\n# edited after the run\n")
    assert main(["verify", str(out)]) == EXIT_FAILED
    printed = capsys.readouterr().out
    assert "FAIL  config hash" in printed  # the strategy's source is part of the config hash
    assert "  ok    result hash" in printed  # a comment does not change the trades


def test_verify_needs_a_run_directory(tmp_path, capsys):
    assert main(["verify", str(tmp_path)]) == EXIT_USAGE
    assert "is this a run directory" in capsys.readouterr().err


# -- walk-forward -----------------------------------------------------------------------


def test_walk_forward_writes_a_report_with_separate_in_and_out_of_sample(demo, tmp_path, capsys):
    def args(out: Path) -> list[str]:
        return [
            "walk-forward", "--strategy", MOMENTUM, "--data", str(demo / "bars.parquet"),
            "--corporate-actions", str(demo / "corporate_actions.csv"),
            "--grid", "lookback=10,30", "--param", "allocation_pct=80",
            "--train-days", "150", "--test-days", "60", "--fee-bps", "1", "--out", str(out),
        ]  # fmt: skip

    out = tmp_path / "wf"
    code = main(args(out))
    assert code == EXIT_OK
    printed = capsys.readouterr().out
    assert "in-sample figures are the best of 2 combinations and are not evidence" in printed

    document = json.loads((out / "walkforward.json").read_text())
    assert document["simulated"] is True and document["combinations"] == 2
    assert document["param_grid"] == {"lookback": [10, 30]}
    folds = document["folds"]
    assert len(folds) >= 3 and document["backtests_run"] == 3 * len(folds)
    for before, after in zip(folds, folds[1:], strict=False):
        assert before["test"][1] == after["test"][0]  # test windows tile without overlap
    for fold in folds:
        assert fold["train"][1] == fold["test"][0]
        assert fold["selected_params"]["lookback"] in (10, 30)
        assert {"in_sample", "out_of_sample"} <= fold.keys()
    assert "## Out-of-sample result" in (out / "walkforward.md").read_text()

    again = tmp_path / "wf2"
    assert main(args(again)) == EXIT_OK
    for name in ("walkforward.json", "walkforward.md"):
        assert (out / name).read_bytes() == (again / name).read_bytes()


# -- errors -----------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("change", "message"),
    [
        (["--strategy", "nope.py"], "nope.py"),
        (["--strategy-class", "Missing"], "Missing"),
        (["--param", "lookback"], "KEY=VALUE"),
        (["--param", "nonsense=1"], "cannot construct Momentum"),
        (["--param", "lookback=0"], "lookback must be at least 1"),
        (["--data", "missing.parquet"], "not found"),
        (["--fee-bps", "abc"], "fee_bps must be a number"),
        (["--fee-bps", "-1"], "fee_bps must be >= 0"),
        (["--participation", "2"], "participation_rate must be <= 1"),
        (["--touch-fill-prob", "1.5"], "touch_fill_probability must be <= 1"),
        (["--latency-ms", "soon"], "--latency-ms expects a number"),
        (["--cash", "0"], "initial_cash must be positive"),
        (["--cash", "lots"], "--cash"),
        (["--oos-start", "someday"], "--oos-start"),
        (["--price-decimals", "12"], "price_decimals must be between 0 and 9"),
    ],
)
def test_bad_input_is_a_usage_error_with_a_message(demo, tmp_path, capsys, change, message):
    args = run_args(demo, tmp_path / "run")
    flag = change[0]
    if flag in args and flag != "--param":
        args[args.index(flag) + 1] = change[1]
    else:
        args += change
    assert main(args) == EXIT_USAGE
    err = capsys.readouterr().err
    assert err.startswith("error: ") and message in err
    assert "Traceback" not in err
    assert not (tmp_path / "run").exists()


def test_a_strategy_file_without_a_strategy_is_refused(demo, tmp_path, capsys):
    empty = tmp_path / "empty.py"
    empty.write_text("x = 1\n")
    args = run_args(demo, tmp_path / "run")
    args[args.index("--strategy") + 1] = str(empty)
    assert main(args) == EXIT_USAGE
    assert "Strategy" in capsys.readouterr().err


def test_walk_forward_argument_errors(demo, tmp_path, capsys):
    base = ["walk-forward", "--strategy", MOMENTUM, "--data", str(demo / "bars.parquet"),
            "--out", str(tmp_path / "wf")]  # fmt: skip
    cases = [
        (["--train-days", "150", "--test-days", "60"], "at least one --grid"),
        (
            ["--train-days", "150", "--test-days", "60", "--grid", "lookback=10",
             "--param", "lookback=5"],
            "both --param and --grid",
        ),
        (["--train-days", "5000", "--test-days", "60", "--grid", "lookback=10"], "too short"),
        (["--train-days", "150", "--test-days", "0", "--grid", "lookback=10"], "test"),
    ]  # fmt: skip
    for extra, message in cases:
        assert main(base + extra) == EXIT_USAGE
        assert message in capsys.readouterr().err
    assert not (tmp_path / "wf").exists()


def test_missing_required_arguments_exit_with_argparse_usage(capsys):
    with pytest.raises(SystemExit) as exit_info:
        main(["run"])
    assert exit_info.value.code == EXIT_USAGE
    assert "--strategy" in capsys.readouterr().err
