"""Fixed-point arithmetic for prices and money.

Prices, cash, notionals, fees and P&L are integers counted in units of
10**-price_decimals of the account currency. Quantities are whole units, so
``price * quantity`` is exact money. Rates such as basis points are exact ratios, and
every rounding direction is chosen explicitly: costs round against the trader.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from decimal import ROUND_HALF_EVEN, Context, Decimal, InvalidOperation
from fractions import Fraction

_CTX = Context(prec=60)


def to_units(value: int | str | float | Decimal, decimals: int) -> int:
    """Convert a currency amount to integer units, rounding half-even at the last unit.

    Integers are whole currency amounts. Floats are accepted only because columnar files
    usually store prices that way; they are rounded once at this boundary and never used
    for arithmetic afterwards.
    """
    if isinstance(value, bool):
        raise TypeError("a boolean is not an amount")
    if isinstance(value, int):
        return value * 10**decimals
    if isinstance(value, float):
        if not math.isfinite(value):
            raise ValueError(f"non-finite amount {value!r}")
        return round(value * 10**decimals)
    try:
        number = value if isinstance(value, Decimal) else Decimal(str(value).strip())
    except InvalidOperation as exc:
        raise ValueError(f"unparseable amount {value!r}") from exc
    if not number.is_finite():
        raise ValueError(f"non-finite amount {value!r}")
    return int(number.scaleb(decimals, _CTX).to_integral_value(ROUND_HALF_EVEN, _CTX))


def fmt_units(units: int, decimals: int) -> str:
    """Render integer units as an exact decimal string."""
    sign = "-" if units < 0 else ""
    if decimals == 0:
        return f"{sign}{abs(units)}"
    whole, frac = divmod(abs(units), 10**decimals)
    return f"{sign}{whole}.{frac:0{decimals}d}"


def div_round_half_even(numerator: int, denominator: int) -> int:
    """Integer division rounding to nearest, ties to even. Exact for any magnitude."""
    if denominator == 0:
        raise ZeroDivisionError("division by zero")
    if denominator < 0:
        numerator, denominator = -numerator, -denominator
    quotient, remainder = divmod(numerator, denominator)
    twice = 2 * remainder
    if twice > denominator or (twice == denominator and quotient % 2 == 1):
        quotient += 1
    return quotient


@dataclass(frozen=True, slots=True)
class Ratio:
    """An exact non-negative rate applied to integers with an explicit rounding direction."""

    num: int
    den: int

    @classmethod
    def parse(cls, value: int | str | float | Decimal | Fraction) -> Ratio:
        if isinstance(value, Fraction):
            frac = value
        else:
            try:
                frac = Fraction(Decimal(str(value)))
            except (InvalidOperation, ValueError) as exc:
                raise ValueError(f"unparseable rate {value!r}") from exc
        if frac < 0:
            raise ValueError(f"rate must be non-negative, got {value!r}")
        return cls(frac.numerator, frac.denominator)

    @classmethod
    def from_bps(cls, value: int | str | float | Decimal) -> Ratio:
        base = cls.parse(value)
        return cls.parse(Fraction(base.num, base.den * 10_000))

    def __bool__(self) -> bool:
        return self.num != 0

    def ceil(self, amount: int) -> int:
        return -((-amount * self.num) // self.den)

    def floor(self, amount: int) -> int:
        return (amount * self.num) // self.den
