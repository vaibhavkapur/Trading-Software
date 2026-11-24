"""Command line interface.

    python -m backtester run --strategy examples/momentum.py --data data/demo/bars.parquet \
        --slippage-bps 3 --fee-bps 1 --seed 42
    python -m backtester walk-forward --strategy examples/momentum.py \
        --data data/demo/bars.parquet --grid lookback=20,40,80 --train-days 365 --test-days 91
    python -m backtester verify runs/<run id>
    python -m backtester gen-data --out data/demo
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from collections.abc import Mapping, Sequence
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Any

from ._version import __version__
from .config import BacktestConfig, ConfigError, ExecutionConfig, RiskLimits
from .data import CALENDARS, DataError, Dataset, load_dataset
from .engine import run_backtest
from .fixedpoint import to_units
from .fixtures import demo_dataset, synthetic_dataset, write_bars, write_corporate_actions
from .report import (
    BANNER,
    deterministic_files,
    pct,
    ratio,
    summary_text,
    walkforward_document,
    write_artifacts,
    write_walkforward,
)
from .strategy import StrategyLoadError, load_strategy_class, strategy_info
from .timeutil import NS_PER_DAY, NS_PER_MS, NS_PER_SEC, fmt_date, parse_ts
from .walkforward import OBJECTIVES, WalkForwardConfig, walk_forward

EXIT_OK = 0
EXIT_FAILED = 1
EXIT_USAGE = 2


class UsageError(ValueError):
    """A command-line value could not be understood."""


# -- argument parsing -------------------------------------------------------------------


def _value(text: str) -> Any:
    """Parse a parameter value as JSON where possible, otherwise keep it as a string."""
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        return text


def _pairs(items: Sequence[str] | None, flag: str) -> list[tuple[str, str]]:
    out = []
    for item in items or ():
        key, sep, value = item.partition("=")
        if not sep or not key.strip():
            raise UsageError(f"{flag} expects KEY=VALUE, got {item!r}")
        out.append((key.strip(), value))
    return out


def _params(items: Sequence[str] | None) -> dict[str, Any]:
    return {key: _value(value) for key, value in _pairs(items, "--param")}


def _grid(items: Sequence[str] | None) -> dict[str, list[Any]]:
    grid = {
        key: [_value(v) for v in value.split(",") if v != ""]
        for key, value in _pairs(items, "--grid")
    }
    if not grid:
        raise UsageError("walk-forward needs at least one --grid KEY=V1,V2,...")
    return grid


def _scaled(text: str | None, scale: int, flag: str) -> int | None:
    """A decimal number of some unit (ms, s, days) as integer nanoseconds."""
    if text is None:
        return None
    try:
        value = Decimal(text) * scale
    except InvalidOperation:
        raise UsageError(f"{flag} expects a number, got {text!r}") from None
    if value < 0 or value != value.to_integral_value():
        raise UsageError(f"{flag} must be a non-negative whole number of nanoseconds")
    return int(value)


def _timestamp(text: str | None, flag: str) -> int | None:
    if text is None:
        return None
    try:
        return parse_ts(text)
    except ValueError as exc:
        raise UsageError(f"{flag}: {exc}") from None


def _money(text: str | None, decimals: int, flag: str) -> int | None:
    if text is None:
        return None
    try:
        return to_units(text, decimals)
    except ValueError as exc:
        raise UsageError(f"{flag}: {exc}") from None


def _add_common(parser: argparse.ArgumentParser) -> None:
    strategy = parser.add_argument_group("strategy")
    strategy.add_argument("--strategy", required=True, help="path to a strategy .py file")
    strategy.add_argument("--strategy-class", help="class name, if the file defines several")
    strategy.add_argument(
        "--param", action="append", metavar="KEY=VALUE", help="strategy parameter (repeatable)"
    )

    data = parser.add_argument_group("data")
    data.add_argument("--data", help="bars file (.csv or .parquet)")
    data.add_argument("--quotes", help="top-of-book quotes file (.csv or .parquet)")
    data.add_argument("--corporate-actions", help="splits and dividends (.csv)")
    data.add_argument("--symbol", help="symbol to use when the file has no symbol column")
    data.add_argument("--bar-seconds", type=int, help="bar length, if the file has no ts_close")
    data.add_argument(
        "--publish-delay-s", default="0", help="delay from event to availability (default 0)"
    )
    data.add_argument("--calendar", choices=CALENDARS, default="weekdays")
    data.add_argument("--price-decimals", type=int, default=4)
    data.add_argument(
        "--on-invalid",
        choices=("raise", "drop"),
        default="raise",
        help="stop on an invalid row (default) or drop it and report the count",
    )

    account = parser.add_argument_group("account")
    account.add_argument("--cash", default="1000000", help="initial cash (default 1000000)")
    account.add_argument("--seed", type=int, default=0)
    account.add_argument("--periods-per-year", type=int, default=252)
    account.add_argument("--risk-free-rate", default="0", help="annual, e.g. 0.03")

    execution = parser.add_argument_group("execution assumptions")
    execution.add_argument("--fee-bps", default="0")
    execution.add_argument("--fee-per-unit", default="0", help="commission per unit traded")
    execution.add_argument("--slippage-bps", default="0")
    execution.add_argument("--spread-bps", default="0", help="assumed full spread for bar data")
    execution.add_argument("--latency-ms", default="0")
    execution.add_argument("--latency-jitter-ms", default="0")
    execution.add_argument(
        "--participation",
        help="max share of bar volume or quote size, e.g. 0.1 (default: unlimited)",
    )
    execution.add_argument("--touch-fill-prob", default="0")
    execution.add_argument("--max-quote-age-ms")

    risk = parser.add_argument_group("risk limits")
    risk.add_argument("--allow-short", action="store_true")
    risk.add_argument("--allow-leverage", action="store_true")
    risk.add_argument("--max-order-qty", type=int)
    risk.add_argument("--max-order-notional")
    risk.add_argument("--max-position-qty", type=int)
    risk.add_argument("--max-open-orders", type=int)
    risk.add_argument("--max-data-age-s", help="refuse orders when the symbol's data is older")

    parser.add_argument("--out", help="output directory (default: runs/<id>)")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m backtester",
        description="Event-driven backtesting engine. Simulation only.",
    )
    parser.add_argument("--version", action="version", version=__version__)
    commands = parser.add_subparsers(dest="command", required=True)

    run = commands.add_parser("run", help="run one backtest and write a report")
    _add_common(run)
    run.add_argument("--oos-start", help="report in-sample and out-of-sample separately")
    run.add_argument("--trade-from", help="warm-up: observe but do not trade before this time")
    run.add_argument("--no-artifacts", action="store_true", help="print the summary only")
    run.set_defaults(handler=_cmd_run)

    wf = commands.add_parser("walk-forward", help="select on train, evaluate on unseen test")
    _add_common(wf)
    wf.add_argument(
        "--grid", action="append", metavar="KEY=V1,V2", help="values to search (repeatable)"
    )
    wf.add_argument("--train-days", required=True, help="training window in calendar days")
    wf.add_argument("--test-days", required=True, help="test window in calendar days")
    wf.add_argument("--step-days", help="distance between folds (default: the test window)")
    wf.add_argument(
        "--anchored", action="store_true", help="training always starts at the beginning"
    )
    wf.add_argument("--objective", choices=OBJECTIVES, default="sharpe")
    wf.set_defaults(handler=_cmd_walk_forward)

    verify = commands.add_parser("verify", help="re-run a saved run and check it reproduces")
    verify.add_argument("run_dir")
    verify.add_argument(
        "--base-dir", default=".", help="directory that the recorded input paths are relative to"
    )
    verify.set_defaults(handler=_cmd_verify)

    gen = commands.add_parser("gen-data", help="write a synthetic dataset (invented prices)")
    gen.add_argument("--out", required=True, help="directory to write into")
    gen.add_argument("--seed", type=int, default=7)
    gen.add_argument("--days", type=int, default=520, help="weekdays of data")
    gen.add_argument("--start", default="2023-01-02")
    gen.add_argument(
        "--symbols",
        help="comma-separated; omit for the demo set with a split, a dividend and a gap",
    )
    gen.add_argument("--bar-seconds", type=int, help="intraday bar length (default: daily bars)")
    gen.set_defaults(handler=_cmd_gen_data)
    return parser


# -- shared construction ----------------------------------------------------------------


def _loader_options(args: argparse.Namespace) -> dict[str, Any]:
    return {
        "price_decimals": args.price_decimals,
        "bar_seconds": args.bar_seconds,
        "publish_delay_ns": _scaled(args.publish_delay_s, NS_PER_SEC, "--publish-delay-s"),
        "default_symbol": args.symbol,
        "calendar": args.calendar,
        "on_invalid": args.on_invalid,
    }


def _config(args: argparse.Namespace) -> BacktestConfig:
    decimals = args.price_decimals
    execution = ExecutionConfig(
        fee_bps=args.fee_bps,
        fee_per_unit=_money(args.fee_per_unit, decimals, "--fee-per-unit"),
        slippage_bps=args.slippage_bps,
        spread_bps=args.spread_bps,
        latency_ns=_scaled(args.latency_ms, NS_PER_MS, "--latency-ms"),
        latency_jitter_ns=_scaled(args.latency_jitter_ms, NS_PER_MS, "--latency-jitter-ms"),
        participation_rate=args.participation,
        touch_fill_probability=args.touch_fill_prob,
        max_quote_age_ns=_scaled(args.max_quote_age_ms, NS_PER_MS, "--max-quote-age-ms"),
    )
    risk = RiskLimits(
        max_order_qty=args.max_order_qty,
        max_order_notional=_money(args.max_order_notional, decimals, "--max-order-notional"),
        max_position_qty=args.max_position_qty,
        max_open_orders=args.max_open_orders,
        max_data_age_ns=_scaled(args.max_data_age_s, NS_PER_SEC, "--max-data-age-s"),
        allow_short=args.allow_short,
        allow_leverage=args.allow_leverage,
    )
    return BacktestConfig(
        initial_cash=_money(args.cash, decimals, "--cash"),
        price_decimals=decimals,
        seed=args.seed,
        execution=execution,
        risk=risk,
        periods_per_year=args.periods_per_year,
        risk_free_rate=args.risk_free_rate,
        trade_from=_timestamp(getattr(args, "trade_from", None), "--trade-from"),
        oos_start=_timestamp(getattr(args, "oos_start", None), "--oos-start"),
    )


def _load(args: argparse.Namespace) -> Dataset:
    return load_dataset(args.data, args.quotes, args.corporate_actions, **_loader_options(args))


def _instantiate(cls: type, params: Mapping[str, Any]):
    try:
        return cls(**params)
    except (TypeError, ValueError) as exc:
        raise StrategyLoadError(
            f"cannot construct {cls.__name__} with {dict(params)}: {exc}"
        ) from exc


# -- commands ---------------------------------------------------------------------------


def _cmd_run(args: argparse.Namespace) -> int:
    params = _params(args.param)
    config = _config(args)
    dataset = _load(args)
    cls = load_strategy_class(args.strategy, args.strategy_class)
    result = run_backtest(
        dataset,
        _instantiate(cls, params),
        config,
        strategy_info=strategy_info(args.strategy, cls, params),
    )
    print(BANNER)
    print()
    print(summary_text(result))
    if not args.no_artifacts:
        inputs = {
            "bars": args.data,
            "quotes": args.quotes,
            "corporate_actions": args.corporate_actions,
            "loader": _loader_options(args),
            "strategy_path": args.strategy,
            "strategy_class": args.strategy_class,
            "params": params,
        }
        out = write_artifacts(result, args.out or Path("runs") / result.run.id, inputs)
        print()
        print(f"  report     {out / 'report.md'}")
        print(f"  verify     python -m backtester verify {out}")
    return EXIT_OK


def _cmd_walk_forward(args: argparse.Namespace) -> int:
    params = _params(args.param)
    grid = _grid(args.grid)
    overlap = sorted(set(params) & set(grid))
    if overlap:
        raise UsageError(f"{', '.join(overlap)} given as both --param and --grid")
    config = _config(args)
    wf_config = WalkForwardConfig(
        train_ns=_scaled(args.train_days, NS_PER_DAY, "--train-days"),
        test_ns=_scaled(args.test_days, NS_PER_DAY, "--test-days"),
        step_ns=_scaled(args.step_days, NS_PER_DAY, "--step-days"),
        param_grid=grid,
        anchored=args.anchored,
        objective=args.objective,
    )
    dataset = _load(args)
    cls = load_strategy_class(args.strategy, args.strategy_class)

    result = walk_forward(
        dataset,
        lambda chosen: _instantiate(cls, {**params, **chosen}),
        config,
        wf_config,
        describe=lambda chosen: strategy_info(args.strategy, cls, {**params, **chosen}),
    )

    value = pct if wf_config.objective == "total_return" else ratio
    name = wf_config.objective.replace("_", " ")
    print(BANNER)
    print()
    print(f"  {len(result.folds)} folds, {result.combinations} combinations, objective: {name}")
    print()
    print(f"  {'fold':<5}{'test window':<26}{'in-sample':>10}{'out-of-sample':>15}  selected")
    for fold in result.folds:
        window = f"{fmt_date(fold.test_start)} to {fmt_date(fold.test_end)}"
        selected = json.dumps(dict(fold.selected.params), sort_keys=True)
        print(
            f"  {fold.index + 1:<5}{window:<26}{value(fold.selected.objective):>10}"
            f"{value(fold.test_objective):>15}  {selected}"
        )
    oos = result.out_of_sample
    print()
    print(
        f"  out-of-sample, all test windows chained: return {pct(oos.total_return)}, "
        f"sharpe {ratio(oos.sharpe)}, max drawdown {pct(oos.max_drawdown)} "
        f"over {oos.periods} days"
    )
    print(
        f"  in-sample figures are the best of {result.combinations} combinations and are "
        "not evidence."
    )
    document = json.dumps(walkforward_document(result), sort_keys=True)
    digest = hashlib.sha256(document.encode()).hexdigest()[:16]
    out = write_walkforward(result, args.out or Path("runs") / f"walkforward-{digest}")
    print()
    print(f"  report     {out / 'walkforward.md'}")
    return EXIT_OK


def _cmd_verify(args: argparse.Namespace) -> int:
    run_dir = Path(args.run_dir)
    manifest_path = run_dir / "manifest.json"
    if not manifest_path.is_file():
        raise UsageError(f"{manifest_path} not found; is this a run directory?")
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    inputs = manifest.get("inputs")
    if not inputs:
        raise UsageError(
            "this run was not made from files, so it cannot be re-run from its manifest"
        )

    base = Path(args.base_dir)

    def resolve(name: str | None) -> Path | None:
        return None if name is None else base / name

    dataset = load_dataset(
        resolve(inputs["bars"]),
        resolve(inputs["quotes"]),
        resolve(inputs["corporate_actions"]),
        **inputs["loader"],
    )
    strategy_path = resolve(inputs["strategy_path"])
    cls = load_strategy_class(strategy_path, inputs["strategy_class"])
    result = run_backtest(
        dataset,
        _instantiate(cls, inputs["params"]),
        BacktestConfig.from_dict(manifest["config"]),
        strategy_info=strategy_info(strategy_path, cls, inputs["params"]),
    )

    checks = [
        ("data version", manifest["data_version"], result.run.data_version),
        ("config hash", manifest["config_hash"], result.run.config_hash),
        ("result hash", manifest["result_hash"], result.result_hash),
    ]
    regenerated = deterministic_files(result)
    for name, recorded in sorted(manifest["artifacts"].items()):
        fresh = (
            hashlib.sha256(regenerated[name].encode()).hexdigest()
            if name in regenerated
            else "missing"
        )
        on_disk = run_dir / name
        stored = (
            hashlib.sha256(on_disk.read_bytes()).hexdigest() if on_disk.is_file() else "missing"
        )
        checks.append((name, recorded, fresh))
        if stored != recorded:
            checks.append((f"{name} (file on disk)", recorded, stored))

    failed = 0
    for name, expected, actual in checks:
        ok = expected == actual
        failed += not ok
        print(f"  {'ok  ' if ok else 'FAIL'}  {name}")
        if not ok:
            print(f"          recorded  {expected}")
            print(f"          now       {actual}")
    print()
    if failed:
        run_id = manifest["run_id"]
        print(f"  NOT REPRODUCED: {failed} of {len(checks)} checks differ from run {run_id}")
        return EXIT_FAILED
    print(f"  REPRODUCED: run {manifest['run_id']} matches on all {len(checks)} checks")
    return EXIT_OK


def _cmd_gen_data(args: argparse.Namespace) -> int:
    if args.symbols:
        symbols = tuple(s.strip() for s in args.symbols.split(",") if s.strip())
        dataset = synthetic_dataset(
            symbols, args.start, args.days, args.seed, bar_seconds=args.bar_seconds
        )
    elif args.bar_seconds or args.start != "2023-01-02":
        raise UsageError("--bar-seconds and --start need --symbols (the demo set is fixed)")
    else:
        dataset = demo_dataset(args.seed, args.days)
    out = Path(args.out)
    written = [write_bars(dataset, out / "bars.csv"), write_bars(dataset, out / "bars.parquet")]
    if dataset.corporate_actions:
        written.append(write_corporate_actions(dataset, out / "corporate_actions.csv"))
    print("Synthetic data: invented prices from a seeded random walk, not market data.")
    print(f"  {len(dataset)} bars, symbols {', '.join(dataset.symbols)}, seed {args.seed}")
    print(f"  data version {dataset.data_version}")
    for path in written:
        print(f"  wrote {path}")
    return EXIT_OK


def main(argv: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return args.handler(args)
    except (UsageError, ConfigError, DataError, StrategyLoadError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_USAGE
    except OSError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_FAILED
