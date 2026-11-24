"""Paths and compact record builders shared by the tests."""

from __future__ import annotations

from decimal import Decimal
from pathlib import Path

from tca.attribution import Analysis, analyze
from tca.benchmark import Methodology
from tca.domain import Bar, ChildOrder, Fill, ParentOrder, Quote, Side
from tca.store import Dataset
from tca.timeutil import parse_ts

ROOT = Path(__file__).resolve().parents[1]
FIXTURE_DIR = ROOT / "examples" / "data"
WORKED_DIR = ROOT / "examples" / "worked"
GOLDEN_REPORT_DIR = ROOT / "examples" / "report"

T0 = parse_ts("2026-03-02T15:00:00+00:00")


def at(seconds: float) -> int:
    """Microsecond timestamp ``seconds`` after 15:00:00 UTC on the fixture date."""
    return T0 + round(seconds * 1_000_000)


def order(order_id="O1", side="buy", qty=1000, *, arrival=0.0, decision=None, **overrides):
    fields = {
        "order_id": order_id,
        "symbol": "XYZ",
        "side": Side(side),
        "qty": qty,
        "decision_ts": at(arrival - 1 if decision is None else decision),
        "arrival_ts": at(arrival),
        "broker": "B1",
        "strategy": "S1",
    }
    return ParentOrder(**(fields | overrides))


def fill(fill_id, qty, price, when, *, order_id="O1", fee="0", venue="V1", child_id=None):
    return Fill(fill_id, order_id, child_id, venue, qty, Decimal(price), Decimal(fee), at(when))


def quote(bid, ask, when, *, venue="V1", symbol="XYZ", recv=None):
    recv_ts = None if recv is None else at(recv)
    return Quote(symbol, venue, Decimal(bid), Decimal(ask), at(when), recv_ts)


def bar(minute, vwap, volume, *, symbol="XYZ", interval_s=60):
    return Bar(symbol, at(minute * 60), interval_s, Decimal(vwap), volume)


def child(child_id, venue, qty, when, *, order_id="O1"):
    return ChildOrder(child_id, order_id, venue, qty, at(when))


def run(
    orders, fills=(), quotes=(), bars=(), child_orders=(), methodology: Methodology | None = None
) -> Analysis:
    """Analyse hand-built records without going through the store."""
    dataset = Dataset(
        orders=tuple(orders),
        child_orders=tuple(child_orders),
        fills=tuple(fills),
        quotes=tuple(quotes),
        bars=tuple(bars),
        versions={},
        version="sha256:test",
    )
    return analyze(dataset, methodology or Methodology())
