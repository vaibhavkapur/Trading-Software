"""Terminal walkthrough: ``python -m tca demo``.

Loads the simulated fixture, prints the headline numbers with the evidence behind them,
then shows how each planted data-quality problem is reported. Every figure printed is
computed from the files in the data directory; nothing is hard-coded.
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

from tca.benchmark import ARRIVAL_MID, BENCHMARKS, INTERVAL_VWAP
from tca.render import BPS_DP, MONEY_DP, PRICE_DP, num, order_view
from tca.report import SIGN_CONVENTION, write_bundle
from tca.service import TcaService

WIDTH = 100


def _heading(text: str) -> None:
    print(f"\n{text}\n{'-' * len(text)}")


def _signed(value: str | None) -> str:
    if value is None:
        return "n/a"
    return value if value.startswith("-") else f"+{value}"


def _table(columns: list[tuple[str, str]], rows: list[dict[str, Any]]) -> None:
    """Print rows as a fixed-width table. ``columns`` is ``[(key, header), ...]``."""
    cells = [[_cell(row.get(key)) for key, _ in columns] for row in rows]
    widths = [
        max(len(header), *(len(line[i]) for line in cells)) if cells else len(header)
        for i, (_, header) in enumerate(columns)
    ]
    headers = [header for _, header in columns]
    for line in [headers, *cells]:
        padded = (cell.ljust(width) for cell, width in zip(line, widths, strict=True))
        print("  " + "  ".join(padded).rstrip())


def _cell(value: Any) -> str:
    if value is None:
        return "-"
    if isinstance(value, list):
        return " ".join(value) or "-"
    return str(value)


def _wrap(text: str, indent: str = "    ") -> None:
    line = indent
    for word in text.split():
        if len(line) + len(word) + 1 > WIDTH and line.strip():
            print(line.rstrip())
            line = indent
        line += word + " "
    print(line.rstrip())


def _summary(service: TcaService, group_by: str, benchmark: str = ARRIVAL_MID) -> None:
    summary = service.summary(group_by, benchmark)
    columns = [
        ("key", group_by),
        ("orders", "orders"),
        ("orders_filled", "filled"),
        ("orders_costed", "costed"),
        ("fill_rate", "fill rate"),
        ("slippage_bps", "slippage bps"),
        ("fee_bps", "fee bps"),
        ("total_bps", "all-in bps"),
        ("effective_spread_bps", "eff. spread bps"),
    ]
    _table(columns, [*summary["rows"], summary["totals"] | {"key": "ALL"}])
    for note in summary["notes"]:
        _wrap(f"note: {note}")


def _order(service: TcaService, order_id: str) -> None:
    analysis = service.analysis()
    result = analysis.by_id.get(order_id)
    if result is None:
        print(f"  {order_id}: not in this dataset")
        return
    view = order_view(result, analysis)
    row = view["summary"]
    print(
        f"  {order_id}  {row['side']} {row['qty']} {row['symbol']}  "
        f"filled {row['filled_qty']}  avg {_cell(row['avg_price'])}"
    )
    for benchmark in view["benchmarks"]:
        name = benchmark["name"]
        trusted = "trusted" if row[f"{name}_trusted"] else "not trusted"
        print(
            f"    {name:<14} {_cell(benchmark['value']):>12}  {benchmark['status']:<9} "
            f"slippage {_signed(row[f'{name}_bps']):>9} bps  ({trusted})"
        )
    print(f"    flags: {_cell(view['flags'])}")
    for note in view["notes"]:
        _wrap(f"- {note}", indent="      ")


def _worked(service: TcaService) -> None:
    """Re-derive one order's arrival shortfall step by step from its fills."""
    analysis = service.analysis()
    result = next(
        (
            r
            for r in analysis.results
            if r.trusted and r.decomposition_trusted and 2 <= len(r.fills) <= 4
        ),
        None,
    )
    if result is None:
        print("  no fully trusted order with 2-4 fills in this dataset")
        return
    order, arrival = result.order, result.benchmarks[ARRIVAL_MID]
    sign = order.side.sign
    details = arrival.details
    print(f"  {order.order_id}: {order.side.value} {order.qty} {order.symbol}")
    print(
        f"  arrival mid = ({details['bid']} + {details['ask']}) / 2 = "
        f"{num(arrival.value, PRICE_DP)}   (side sign {sign:+d})"
    )
    total = 0
    for f in result.fills:
        cost = sign * f.fill.qty * (f.fill.price - arrival.value)
        total += cost
        print(
            f"    {sign:+d} x {f.fill.qty} x ({f.fill.price} - {num(arrival.value, PRICE_DP)})"
            f" = {_signed(num(cost, MONEY_DP))}"
        )
    slippage = result.slippage[ARRIVAL_MID]
    print(
        f"  shortfall = {_signed(num(total, MONEY_DP))} over notional "
        f"{num(slippage.notional, MONEY_DP)} = {_signed(num(slippage.bps, BPS_DP))} bps"
    )
    verdict = "match" if total == slippage.cost else "MISMATCH"
    print(f"  engine reports {_signed(num(slippage.cost, MONEY_DP))}: {verdict}")
    print(
        f"  split: spread {_signed(num(result.spread_cost, MONEY_DP))} + drift "
        f"{_signed(num(result.drift_cost, MONEY_DP))}; fees {_signed(num(result.fees, MONEY_DP))}"
        f"; all-in {_signed(num(result.total_cost, MONEY_DP))}"
    )


def _first_with(service: TcaService, flag: str) -> str | None:
    return next(
        (r.order.order_id for r in service.analysis().results if flag in r.flags and r.fills),
        None,
    )


def run(data_dir: Path, out_dir: Path) -> int:
    service = TcaService()
    print("Transaction Cost Analysis demo - simulated data only, no live connectivity")

    _heading(f"1. Import {data_dir}")
    results = service.import_directory(data_dir)
    for result in results:
        print(
            f"  {result.dataset:<13} rows={result.rows:<6} accepted={result.accepted:<6} "
            f"duplicates={result.duplicates:<3} rejected={result.rejected}"
        )
    analysis = service.analysis()
    print(f"  dataset version      {analysis.dataset.version}")
    print(f"  methodology version  {analysis.methodology.id}  {analysis.methodology.params}")
    _wrap(SIGN_CONVENTION, indent="  ")

    _heading("2. One order, by hand")
    _worked(service)

    _heading("3. Arrival-price cost by strategy (trusted benchmarks only)")
    _summary(service, "strategy")

    _heading("4. Arrival-price cost by venue")
    _summary(service, "venue")

    _heading("5. How much of this can be trusted")
    filled = [r for r in analysis.results if r.fills]
    for name in BENCHMARKS:
        trusted = sum(r.slippage[name].trusted for r in filled)
        print(f"  {name:<14} {trusted} of {len(filled)} filled orders trusted")
    print(f"  orphan fills: {analysis.orphan_fills}")
    untrusted = service.summary("all", include_untrusted=True)["totals"]
    trusted_totals = service.summary("all")["totals"]
    print(
        f"  slippage vs arrival mid: {_signed(trusted_totals['slippage_bps'])} bps trusted only, "
        f"{_signed(untrusted['slippage_bps'])} bps if untrusted benchmarks are included"
    )

    _heading("6. Failure cases")
    cases = [
        ("ARRIVAL_QUOTE_MISSING", "no quote yet at arrival (feed started late)"),
        ("ARRIVAL_QUOTE_STALE", "arrival during a quote outage"),
        ("ARRIVAL_CLOCK_SKEW", "venue clock running ahead of receive time"),
        ("VWAP_BARS_PARTIAL", "bars missing inside the order's interval"),
    ]
    for flag, title in cases:
        order_id = _first_with(service, flag)
        print(f"\n  {flag}: {title}")
        if order_id is None:
            print("    no filled order carries this flag in this dataset")
        else:
            _order(service, order_id)

    print("\n  Duplicate delivery: importing the same files a second time")
    before = analysis.dataset.version
    again = service.import_directory(data_dir)
    print(
        f"    accepted={sum(r.accepted for r in again)} "
        f"duplicates={sum(r.duplicates for r in again)} rejected={sum(r.rejected for r in again)}"
    )
    after = service.analysis().dataset.version
    print(f"    dataset version {'unchanged' if after == before else 'CHANGED'}")

    _heading(f"7. Report bundle -> {out_dir}")
    manifest = write_bundle(service.analysis(), out_dir)
    print(f"  report id    {manifest['report_id']}")
    print(f"  methodology  {manifest['methodology']['id']}")
    print(f"  dataset      {manifest['dataset']['version']}")
    for name, info in manifest["files"].items():
        print(f"  {name:<28} {info['rows']:>5} rows  {info['sha256'][:23]}...")
    vwap_trusted = manifest["quality"]["orders_trusted"][INTERVAL_VWAP]
    print(
        f"  manifest.json records the above plus {vwap_trusted} VWAP-trusted orders and flag counts"
    )
    return 0 if after == before else 1
