"""Command line entry point: ``python -m tca <command>``."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from tca.benchmark import BENCHMARKS
from tca.config import load_settings
from tca.importer import ImportFormatError
from tca.report import write_bundle
from tca.service import TcaService
from tca.simulator import DEFAULT_SEED, write_fixture
from tca.store import Store


def _print_imports(results) -> int:
    rejected = 0
    for result in results:
        print(
            f"{result.dataset:<13} rows={result.rows:<6} accepted={result.accepted:<6} "
            f"duplicates={result.duplicates:<6} rejected={result.rejected}"
        )
        for reject in result.rejects[:10]:
            print(f"    row {reject.row}: {reject.code}: {reject.detail}")
        if result.rejected > 10:
            print(f"    ... and {result.rejected - 10} more")
        rejected += result.rejected
    return rejected


def _service(db_path: str) -> TcaService:
    return TcaService(Store(db_path), load_settings().methodology)


def cmd_serve(args: argparse.Namespace) -> int:
    import uvicorn

    from tca.api import create_app

    settings = load_settings()
    service = _service(args.db or settings.db_path)
    if args.load:
        _print_imports(service.import_directory(Path(args.load)))
    uvicorn.run(
        create_app(service), host=args.host or settings.host, port=args.port or settings.port
    )
    return 0


def cmd_import(args: argparse.Namespace) -> int:
    service = _service(args.db)
    rejected = _print_imports(service.import_directory(Path(args.directory)))
    print(f"dataset version {service.analysis().dataset.version}")
    return 1 if rejected and args.strict else 0


def cmd_report(args: argparse.Namespace) -> int:
    service = _service(args.db or ":memory:")
    if args.data:
        _print_imports(service.import_directory(Path(args.data)))
    analysis = service.analysis()
    if not analysis.results:
        print("no orders to report on; pass --data DIR or --db PATH", file=sys.stderr)
        return 1
    manifest = write_bundle(
        analysis,
        Path(args.out),
        benchmark=args.benchmark,
        include_untrusted=args.include_untrusted,
        bucket_minutes=args.bucket_minutes,
    )
    print(f"report {manifest['report_id']} written to {args.out}")
    print(f"  methodology {manifest['methodology']['id']}")
    print(f"  dataset     {manifest['dataset']['version']}")
    print(
        f"  quality     {json.dumps(manifest['quality']['orders_trusted'])} trusted of "
        f"{manifest['quality']['orders_filled']} filled orders"
    )
    return 0


def cmd_simulate(args: argparse.Namespace) -> int:
    counts = write_fixture(Path(args.out), args.seed)
    print(f"simulated fixture (seed {args.seed}) written to {args.out}: {counts}")
    return 0


def cmd_demo(args: argparse.Namespace) -> int:
    from tca.demo import run

    return run(Path(args.data), Path(args.out))


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="tca", description=__doc__)
    commands = parser.add_subparsers(dest="command")

    serve = commands.add_parser("serve", help="run the API and dashboard")
    serve.add_argument("--host")
    serve.add_argument("--port", type=int)
    serve.add_argument("--db", help="SQLite path (default: TCA_DB_PATH or in-memory)")
    serve.add_argument("--load", metavar="DIR", help="import dataset files from DIR at startup")
    serve.set_defaults(run=cmd_serve)

    load = commands.add_parser("import", help="import dataset files into a SQLite database")
    load.add_argument("directory")
    load.add_argument("--db", required=True)
    load.add_argument("--strict", action="store_true", help="exit 1 if any row is rejected")
    load.set_defaults(run=cmd_import)

    report = commands.add_parser("report", help="write a versioned report bundle")
    report.add_argument("--data", metavar="DIR", help="import dataset files from DIR first")
    report.add_argument("--db", help="SQLite path to read from (default: in-memory)")
    report.add_argument("--out", required=True)
    report.add_argument("--benchmark", choices=BENCHMARKS, default="arrival_mid")
    report.add_argument("--include-untrusted", action="store_true")
    report.add_argument("--bucket-minutes", type=int, default=30)
    report.set_defaults(run=cmd_report)

    simulate = commands.add_parser("simulate", help="regenerate the simulated fixture dataset")
    simulate.add_argument("--out", default="examples/data")
    simulate.add_argument("--seed", type=int, default=DEFAULT_SEED)
    simulate.set_defaults(run=cmd_simulate)

    demo = commands.add_parser("demo", help="terminal walkthrough on the simulated fixture")
    demo.add_argument("--data", default="examples/data")
    demo.add_argument("--out", default="reports/demo")
    demo.set_defaults(run=cmd_demo)

    args = parser.parse_args(argv)
    if args.command is None:
        args = parser.parse_args(["serve"])
    try:
        return args.run(args)
    except (ImportFormatError, FileNotFoundError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
