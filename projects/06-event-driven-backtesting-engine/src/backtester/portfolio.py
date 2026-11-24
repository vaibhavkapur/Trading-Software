"""Portfolio accounting: cash, positions, P&L and the transaction ledger.

Positions use average-cost accounting on integers. ``cost_basis`` is the signed total
paid for the open quantity (negative for a short), so no per-unit average is ever stored
and no rounding error can accumulate. The identity

    equity = initial_cash + realized_pnl + unrealized_pnl - fees + dividends

holds exactly after every transaction, and ``reconcile`` re-derives every balance from
the ledger and the fill list to prove it.
"""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass
from enum import StrEnum

from .fixedpoint import div_round_half_even
from .orders import SimFill


class LedgerKind(StrEnum):
    DEPOSIT = "deposit"
    TRADE = "trade"
    FEE = "fee"
    DIVIDEND = "dividend"
    SPLIT = "split"
    CASH_IN_LIEU = "cash_in_lieu"


@dataclass(frozen=True, slots=True)
class LedgerEntry:
    """One balance-changing transaction. ``qty`` and ``amount`` are signed deltas."""

    seq: int
    ts: int
    kind: LedgerKind
    symbol: str
    qty: int
    px: int
    amount: int
    cash_after: int
    ref: str


@dataclass(frozen=True, slots=True)
class PortfolioSnapshot:
    """``holdings`` is a tuple of (symbol, quantity, mark price), sorted by symbol."""

    ts: int
    cash: int
    holdings: tuple[tuple[str, int, int], ...]
    equity: int


@dataclass(slots=True)
class Position:
    qty: int = 0
    cost_basis: int = 0
    realized_pnl: int = 0


class ReconciliationError(RuntimeError):
    """Balances do not agree with the ledger and fills. This is always an engine bug."""


class Portfolio:
    def __init__(self, initial_cash: int, ts: int = 0) -> None:
        self.initial_cash = initial_cash
        self.cash = 0
        self.fees = 0
        self.dividends = 0
        self.positions: dict[str, Position] = {}
        self.marks: dict[str, int] = {}
        self.ledger: list[LedgerEntry] = []
        self._post(ts, LedgerKind.DEPOSIT, "", 0, 0, initial_cash, "initial")

    # -- transactions ---------------------------------------------------------------

    def apply_fill(self, fill: SimFill) -> None:
        signed = fill.side.sign * fill.qty
        position = self.positions.setdefault(fill.symbol, Position())
        self._trade(position, signed, fill.px)
        self._post(
            fill.ts, LedgerKind.TRADE, fill.symbol, signed, fill.px, -signed * fill.px, fill.id
        )
        if fill.fee:
            self.fees += fill.fee
            self._post(fill.ts, LedgerKind.FEE, fill.symbol, 0, 0, -fill.fee, fill.id)
        # Until market data for this symbol is visible, the fill is the best known price.
        self.marks.setdefault(fill.symbol, fill.px)

    def apply_dividend(self, symbol: str, amount_per_unit: int, ts: int) -> int:
        """Credit (or, for a short, debit) a cash dividend on the quantity held."""
        position = self.positions.get(symbol)
        if position is None or position.qty == 0:
            return 0
        amount = position.qty * amount_per_unit
        self.dividends += amount
        self._post(ts, LedgerKind.DIVIDEND, symbol, 0, amount_per_unit, amount, "dividend")
        return amount

    def apply_split(self, symbol: str, num: int, den: int, ts: int) -> None:
        """Apply a ``num``-for-``den`` split. Fractional units are settled in cash."""
        if num <= 0 or den <= 0:
            raise ValueError("split ratio must be positive")
        old_mark = self.marks.get(symbol)
        if old_mark is not None:
            self.marks[symbol] = max(1, div_round_half_even(old_mark * den, num))
        position = self.positions.get(symbol)
        if position is None or position.qty == 0:
            return

        old_qty = position.qty
        scaled = old_qty * num
        new_qty = abs(scaled) // den * (1 if scaled > 0 else -1)
        remainder = scaled - new_qty * den  # fractional part, in units of 1/den, signed
        position.qty = new_qty
        self._post(ts, LedgerKind.SPLIT, symbol, new_qty - old_qty, 0, 0, f"{num}:{den}")
        if remainder:
            # remainder/den new units are worth remainder * old_mark / num.
            cash = div_round_half_even(remainder * (old_mark or 0), num)
            cost = div_round_half_even(position.cost_basis * remainder, scaled)
            position.cost_basis -= cost
            position.realized_pnl += cash - cost
            self._post(ts, LedgerKind.CASH_IN_LIEU, symbol, 0, 0, cash, f"{num}:{den}")

    def mark(self, symbol: str, px: int) -> None:
        self.marks[symbol] = px

    # -- views ----------------------------------------------------------------------

    def position_qty(self, symbol: str) -> int:
        position = self.positions.get(symbol)
        return position.qty if position else 0

    def market_value(self) -> int:
        marks = self.marks
        return sum(p.qty * marks[s] for s, p in self.positions.items() if p.qty)

    def equity(self) -> int:
        return self.cash + self.market_value()

    def realized_pnl(self) -> int:
        return sum(p.realized_pnl for p in self.positions.values())

    def unrealized_pnl(self) -> int:
        marks = self.marks
        return sum(p.qty * marks[s] - p.cost_basis for s, p in self.positions.items() if p.qty)

    def snapshot(self, ts: int) -> PortfolioSnapshot:
        marks = self.marks
        holdings = tuple(sorted((s, p.qty, marks[s]) for s, p in self.positions.items() if p.qty))
        equity = self.cash + sum(qty * px for _, qty, px in holdings)
        return PortfolioSnapshot(ts, self.cash, holdings, equity)

    # -- reconciliation -------------------------------------------------------------

    def reconcile(self, fills: Iterable[SimFill]) -> dict[str, int]:
        """Re-derive every balance independently and raise if anything disagrees."""
        running = 0
        ledger_qty: dict[str, int] = {}
        totals = dict.fromkeys(LedgerKind, 0)
        for index, entry in enumerate(self.ledger):
            if entry.seq != index:
                raise ReconciliationError(f"ledger sequence broken at entry {index}")
            running += entry.amount
            if running != entry.cash_after:
                raise ReconciliationError(f"ledger running balance broken at entry {index}")
            totals[entry.kind] += entry.amount
            if entry.qty:
                ledger_qty[entry.symbol] = ledger_qty.get(entry.symbol, 0) + entry.qty
        if running != self.cash:
            raise ReconciliationError(f"cash {self.cash} != ledger balance {running}")

        fill_cash = 0
        fill_fees = 0
        fill_count = 0
        fill_qty: dict[str, int] = {}
        for fill in fills:
            signed = fill.side.sign * fill.qty
            fill_cash -= signed * fill.px
            fill_fees += fill.fee
            fill_qty[fill.symbol] = fill_qty.get(fill.symbol, 0) + signed
            fill_count += 1
        if fill_cash != totals[LedgerKind.TRADE]:
            raise ReconciliationError("trade cash in ledger does not match the fills")
        if fill_fees != -totals[LedgerKind.FEE] or fill_fees != self.fees:
            raise ReconciliationError("fees in ledger do not match the fills")

        expected_cash = (
            self.initial_cash
            + fill_cash
            - fill_fees
            + totals[LedgerKind.DIVIDEND]
            + totals[LedgerKind.CASH_IN_LIEU]
        )
        if expected_cash != self.cash:
            raise ReconciliationError(f"cash {self.cash} != {expected_cash} derived from fills")
        if totals[LedgerKind.DIVIDEND] != self.dividends:
            raise ReconciliationError("dividends do not match the ledger")

        split_qty: dict[str, int] = {}
        for entry in self.ledger:
            if entry.kind is LedgerKind.SPLIT:
                split_qty[entry.symbol] = split_qty.get(entry.symbol, 0) + entry.qty
        for symbol in sorted(set(self.positions) | set(ledger_qty) | set(fill_qty)):
            held = self.position_qty(symbol)
            if held != ledger_qty.get(symbol, 0):
                raise ReconciliationError(f"{symbol}: position {held} != ledger quantity")
            if held != fill_qty.get(symbol, 0) + split_qty.get(symbol, 0):
                raise ReconciliationError(f"{symbol}: position {held} != fills plus splits")
            position = self.positions.get(symbol)
            if position and position.qty == 0 and position.cost_basis != 0:
                raise ReconciliationError(f"{symbol}: flat position still carries cost basis")

        equity = self.equity()
        realized = self.realized_pnl()
        unrealized = self.unrealized_pnl()
        identity = self.initial_cash + realized + unrealized - self.fees + self.dividends
        if equity != identity:
            raise ReconciliationError(f"equity {equity} != accounting identity {identity}")
        return {
            "ledger_entries": len(self.ledger),
            "fills": fill_count,
            "cash": self.cash,
            "equity": equity,
            "realized_pnl": realized,
            "unrealized_pnl": unrealized,
            "fees": self.fees,
            "dividends": self.dividends,
        }

    # -- internals ------------------------------------------------------------------

    def _trade(self, position: Position, signed: int, px: int) -> None:
        held = position.qty
        if held == 0 or (held > 0) == (signed > 0):
            position.qty = held + signed
            position.cost_basis += signed * px
            return
        closing = min(abs(signed), abs(held)) * (1 if signed > 0 else -1)
        if closing == -held:
            cost = position.cost_basis
        else:
            cost = div_round_half_even(position.cost_basis * abs(closing), abs(held))
        position.realized_pnl += -closing * px - cost
        position.cost_basis -= cost
        position.qty = held + closing
        opening = signed - closing
        if opening:
            position.qty = opening
            position.cost_basis = opening * px

    def _post(
        self, ts: int, kind: LedgerKind, symbol: str, qty: int, px: int, amount: int, ref: str
    ) -> None:
        self.cash += amount
        self.ledger.append(
            LedgerEntry(len(self.ledger), ts, kind, symbol, qty, px, amount, self.cash, ref)
        )
