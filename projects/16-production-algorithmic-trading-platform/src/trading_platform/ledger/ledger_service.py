"""Double-entry accounting ledger and average-cost position tracking."""

from __future__ import annotations

import uuid
from decimal import Decimal

from trading_platform.domain.events import (
    FillEvent,
    JournalLine,
    LedgerEntry,
    PositionRecord,
    Side,
    utc_now,
)
from trading_platform.storage.sqlite_store import SQLiteStore

ACCT_CASH = "1000-CASH"
ACCT_INVENTORY = "1200-SECURITIES-INVENTORY"
ACCT_SHORT_LIABILITY = "2000-SHORT-LIABILITY"
ACCT_EQUITY_CAPITAL = "3000-EQUITY-CAPITAL"
ACCT_REALIZED_PNL = "4000-REALIZED-PNL"
ACCT_COMMISSION_EXPENSE = "5000-COMMISSION-EXPENSE"

Q4 = Decimal("0.0001")


def _q(val: Decimal) -> Decimal:
    return val.quantize(Q4)


class LedgerService:
    """Strict double-entry general ledger (debits == credits per currency) and position book."""

    def __init__(
        self,
        store: SQLiteStore,
        *,
        account_id: str,
        base_currency: str = "USD",
    ) -> None:
        self.store = store
        self.account_id = account_id
        self.base_currency = base_currency

    def initialize_capital(self, initial_cash: Decimal) -> LedgerEntry | None:
        """Idempotently books opening capital deposit."""
        amt = _q(initial_cash)
        if amt <= 0:
            return None
        entry = LedgerEntry(
            entry_id=f"JNL-{uuid.uuid4().hex[:12].upper()}",
            idempotency_key=f"INIT_CAPITAL:{self.account_id}:{self.base_currency}",
            account_id=self.account_id,
            entry_type="CASH_DEPOSIT",
            reference_id=self.account_id,
            description=f"Opening paper capital deposit for {self.account_id}",
            lines=[
                JournalLine(
                    account_code=ACCT_CASH,
                    currency=self.base_currency,
                    debit=amt,
                    memo="Initial paper cash funding",
                ),
                JournalLine(
                    account_code=ACCT_EQUITY_CAPITAL,
                    currency=self.base_currency,
                    credit=amt,
                    memo="Initial paper equity capital",
                ),
            ],
        )
        inserted = self.store.save_ledger_entry_if_absent(entry)
        return entry if inserted else None

    def post_fill(self, fill: FillEvent) -> tuple[LedgerEntry | None, PositionRecord]:
        """Idempotently books a trade fill into the double-entry ledger and updates position."""
        idem_key = f"FILL:{fill.fill_id}"
        existing_pos = self.store.get_position(fill.account_id, fill.symbol) or PositionRecord(
            account_id=fill.account_id,
            symbol=fill.symbol,
            currency=fill.currency,
        )

        gross = _q(fill.quantity * fill.price)
        comm = _q(fill.commission)
        signed_delta = fill.quantity if fill.side == Side.BUY else -fill.quantity

        old_qty = existing_pos.quantity
        old_avg = existing_pos.average_cost
        new_qty = old_qty + signed_delta

        lines: list[JournalLine] = []
        realized_pnl_delta = Decimal("0")

        if fill.side == Side.BUY:
            # Cash decreases by gross + commission
            cash_out = _q(gross + comm)
            lines.append(
                JournalLine(
                    account_code=ACCT_CASH,
                    currency=fill.currency,
                    credit=cash_out,
                    symbol=fill.symbol,
                    memo=f"Cash paid for BUY {fill.quantity} {fill.symbol} @ {fill.price}",
                )
            )
            if comm > 0:
                lines.append(
                    JournalLine(
                        account_code=ACCT_COMMISSION_EXPENSE,
                        currency=fill.currency,
                        debit=comm,
                        symbol=fill.symbol,
                        memo=f"Commission for {fill.fill_id}",
                    )
                )

            if old_qty >= 0:
                # Opening or adding to long position
                lines.append(
                    JournalLine(
                        account_code=ACCT_INVENTORY,
                        currency=fill.currency,
                        debit=gross,
                        symbol=fill.symbol,
                        memo=f"Add long inventory {fill.symbol}",
                    )
                )
                total_cost = (old_qty * old_avg) + gross
                new_avg = _q(total_cost / new_qty) if new_qty != 0 else Decimal("0")
            else:
                # Covering short (possibly flipping to long)
                cover_qty = min(fill.quantity, abs(old_qty))
                open_long_qty = fill.quantity - cover_qty
                cover_basis = _q(cover_qty * old_avg)
                cover_cost = _q(cover_qty * fill.price)
                realized_pnl_delta = _q(cover_basis - cover_cost)

                lines.append(
                    JournalLine(
                        account_code=ACCT_SHORT_LIABILITY,
                        currency=fill.currency,
                        debit=cover_basis,
                        symbol=fill.symbol,
                        memo=f"Cover short liability {cover_qty} {fill.symbol}",
                    )
                )
                if open_long_qty > 0:
                    open_long_cost = _q(gross - cover_cost)
                    lines.append(
                        JournalLine(
                            account_code=ACCT_INVENTORY,
                            currency=fill.currency,
                            debit=open_long_cost,
                            symbol=fill.symbol,
                            memo=f"Flip to long inventory {open_long_qty} {fill.symbol}",
                        )
                    )
                    new_avg = fill.price
                elif new_qty == 0:
                    new_avg = Decimal("0")
                else:
                    new_avg = old_avg

                if realized_pnl_delta > 0:
                    lines.append(
                        JournalLine(
                            account_code=ACCT_REALIZED_PNL,
                            currency=fill.currency,
                            credit=realized_pnl_delta,
                            symbol=fill.symbol,
                            memo=f"Realized gain covering short {fill.symbol}",
                        )
                    )
                elif realized_pnl_delta < 0:
                    lines.append(
                        JournalLine(
                            account_code=ACCT_REALIZED_PNL,
                            currency=fill.currency,
                            debit=abs(realized_pnl_delta),
                            symbol=fill.symbol,
                            memo=f"Realized loss covering short {fill.symbol}",
                        )
                    )
        else:
            # SELL: Cash increases by gross - commission
            net_cash = _q(gross - comm)
            if net_cash >= 0:
                lines.append(
                    JournalLine(
                        account_code=ACCT_CASH,
                        currency=fill.currency,
                        debit=net_cash,
                        symbol=fill.symbol,
                        memo=f"Cash received for SELL {fill.quantity} {fill.symbol} @ {fill.price}",
                    )
                )
            else:
                lines.append(
                    JournalLine(
                        account_code=ACCT_CASH,
                        currency=fill.currency,
                        credit=abs(net_cash),
                        symbol=fill.symbol,
                        memo=f"Net cash outflow on small SELL {fill.symbol}",
                    )
                )
            if comm > 0:
                lines.append(
                    JournalLine(
                        account_code=ACCT_COMMISSION_EXPENSE,
                        currency=fill.currency,
                        debit=comm,
                        symbol=fill.symbol,
                        memo=f"Commission for {fill.fill_id}",
                    )
                )

            if old_qty <= 0:
                # Opening or adding to short position
                lines.append(
                    JournalLine(
                        account_code=ACCT_SHORT_LIABILITY,
                        currency=fill.currency,
                        credit=gross,
                        symbol=fill.symbol,
                        memo=f"Add short liability {fill.symbol}",
                    )
                )
                total_short_proceeds = (abs(old_qty) * old_avg) + gross
                new_avg = _q(total_short_proceeds / abs(new_qty)) if new_qty != 0 else Decimal("0")
            else:
                # Closing long (possibly flipping to short)
                close_qty = min(fill.quantity, old_qty)
                open_short_qty = fill.quantity - close_qty
                close_basis = _q(close_qty * old_avg)
                close_proceeds = _q(close_qty * fill.price)
                realized_pnl_delta = _q(close_proceeds - close_basis)

                lines.append(
                    JournalLine(
                        account_code=ACCT_INVENTORY,
                        currency=fill.currency,
                        credit=close_basis,
                        symbol=fill.symbol,
                        memo=f"Release long inventory {close_qty} {fill.symbol}",
                    )
                )
                if open_short_qty > 0:
                    open_short_proceeds = _q(gross - close_proceeds)
                    lines.append(
                        JournalLine(
                            account_code=ACCT_SHORT_LIABILITY,
                            currency=fill.currency,
                            credit=open_short_proceeds,
                            symbol=fill.symbol,
                            memo=f"Flip to short liability {open_short_qty} {fill.symbol}",
                        )
                    )
                    new_avg = fill.price
                elif new_qty == 0:
                    new_avg = Decimal("0")
                else:
                    new_avg = old_avg

                if realized_pnl_delta > 0:
                    lines.append(
                        JournalLine(
                            account_code=ACCT_REALIZED_PNL,
                            currency=fill.currency,
                            credit=realized_pnl_delta,
                            symbol=fill.symbol,
                            memo=f"Realized gain selling long {fill.symbol}",
                        )
                    )
                elif realized_pnl_delta < 0:
                    lines.append(
                        JournalLine(
                            account_code=ACCT_REALIZED_PNL,
                            currency=fill.currency,
                            debit=abs(realized_pnl_delta),
                            symbol=fill.symbol,
                            memo=f"Realized loss selling long {fill.symbol}",
                        )
                    )

        entry = LedgerEntry(
            entry_id=f"JNL-{uuid.uuid4().hex[:12].upper()}",
            idempotency_key=idem_key,
            account_id=fill.account_id,
            entry_type="FILL",
            reference_id=fill.fill_id,
            description=f"{fill.side.value} {fill.quantity} {fill.symbol} @ {fill.price}",
            posted_at_utc=fill.executed_at_utc,
            lines=lines,
        )
        inserted = self.store.save_ledger_entry_if_absent(entry)
        if not inserted:
            return None, existing_pos

        updated_pos = PositionRecord(
            account_id=fill.account_id,
            symbol=fill.symbol,
            currency=fill.currency,
            quantity=new_qty,
            average_cost=new_avg,
            realized_pnl=_q(existing_pos.realized_pnl + realized_pnl_delta),
            total_commissions=_q(existing_pos.total_commissions + comm),
            updated_at_utc=utc_now(),
        )
        self.store.save_position(updated_pos)
        return entry, updated_pos

    def trial_balance(self) -> dict[str, dict[str, Decimal]]:
        """Returns {account_code: {'debits': ..., 'credits': ..., 'net': debits - credits}}."""
        totals: dict[str, dict[str, Decimal]] = {}
        for entry in self.store.list_ledger_entries():
            for line in entry.lines:
                bucket = totals.setdefault(
                    line.account_code,
                    {"debits": Decimal("0"), "credits": Decimal("0"), "net": Decimal("0")},
                )
                bucket["debits"] += line.debit
                bucket["credits"] += line.credit
                bucket["net"] = bucket["debits"] - bucket["credits"]
        return totals

    def is_balanced(self) -> bool:
        tb = self.trial_balance()
        total_dr = sum((v["debits"] for v in tb.values()), Decimal("0"))
        total_cr = sum((v["credits"] for v in tb.values()), Decimal("0"))
        return total_dr == total_cr

    def cash_balance(self) -> Decimal:
        tb = self.trial_balance()
        if ACCT_CASH not in tb:
            return Decimal("0")
        return _q(tb[ACCT_CASH]["net"])

    def total_commissions(self) -> Decimal:
        tb = self.trial_balance()
        if ACCT_COMMISSION_EXPENSE not in tb:
            return Decimal("0")
        return _q(tb[ACCT_COMMISSION_EXPENSE]["net"])

    def total_realized_pnl(self) -> Decimal:
        tb = self.trial_balance()
        if ACCT_REALIZED_PNL not in tb:
            return Decimal("0")
        # Revenue/PnL account is credit-normal
        return _q(-tb[ACCT_REALIZED_PNL]["net"])

    def positions_map(self) -> dict[str, Decimal]:
        result: dict[str, Decimal] = {}
        for pos in self.store.list_positions(self.account_id):
            if pos.quantity != 0:
                result[pos.symbol] = pos.quantity
        return result

    def unrealized_pnl(self, mark_prices: dict[str, Decimal]) -> Decimal:
        total = Decimal("0")
        for pos in self.store.list_positions(self.account_id):
            if pos.quantity == 0:
                continue
            mark = mark_prices.get(pos.symbol, pos.average_cost)
            total += pos.quantity * (mark - pos.average_cost)
        return _q(total)

    def gross_position_notional(self, mark_prices: dict[str, Decimal]) -> Decimal:
        total = Decimal("0")
        for pos in self.store.list_positions(self.account_id):
            if pos.quantity == 0:
                continue
            mark = mark_prices.get(pos.symbol, pos.average_cost)
            total += abs(pos.quantity) * mark
        return _q(total)

    def net_daily_pnl(self, mark_prices: dict[str, Decimal]) -> Decimal:
        return _q(
            self.total_realized_pnl()
            + self.unrealized_pnl(mark_prices)
            - self.total_commissions()
        )
