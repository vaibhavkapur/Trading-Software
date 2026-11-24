"""Commission model. Fees always round up to the next price unit."""

from __future__ import annotations

from decimal import Decimal

from .fixedpoint import Ratio


class FeeModel:
    __slots__ = ("_bps", "_per_unit")

    def __init__(self, fee_bps: Decimal | int | str = 0, fee_per_unit: int = 0) -> None:
        self._bps = Ratio.from_bps(fee_bps)
        self._per_unit = fee_per_unit

    def fee(self, notional: int, qty: int) -> int:
        return self._bps.ceil(notional) + qty * self._per_unit
