"""Fixed-point Decimal transaction cost and slippage model.

Avoids floating-point currency arithmetic in the execution path. Computes
commission, half-spread, and participation-based market impact in Decimal USD.
"""

from __future__ import annotations

from dataclasses import dataclass
from decimal import ROUND_HALF_UP, Decimal

from quant_agent.models.signals import TransactionCostAssumptions

CENT = Decimal("0.01")
PRICE_TICK = Decimal("0.0001")
BPS_DIVISOR = Decimal("10000")


@dataclass(frozen=True)
class FillCostBreakdown:
    fill_price: Decimal
    notional: Decimal
    commission: Decimal
    spread_cost: Decimal
    impact_cost: Decimal
    total_cost: Decimal


class TransactionCostModel:
    """Deterministic execution cost model using fixed-point Decimal math."""

    def __init__(self, assumptions: TransactionCostAssumptions) -> None:
        self.commission_bps = Decimal(str(assumptions.commission_bps))
        self.half_spread_bps = Decimal(str(assumptions.half_spread_bps))
        self.impact_bps_per_pct_adv = Decimal(str(assumptions.impact_bps_per_pct_adv))
        self.borrow_fee_annual_bps = Decimal(str(assumptions.borrow_fee_annual_bps))

    def estimate_fill(
        self,
        *,
        side: str,
        quantity: int,
        reference_price: Decimal,
        trailing_adv_shares: int,
    ) -> FillCostBreakdown:
        if quantity <= 0:
            raise ValueError("quantity must be > 0")
        if reference_price <= Decimal("0"):
            raise ValueError("reference_price must be > 0")

        qty_dec = Decimal(quantity)
        notional = (reference_price * qty_dec).quantize(CENT, rounding=ROUND_HALF_UP)

        adv = max(1, trailing_adv_shares)
        # Participation rate in percent of ADV (e.g., 1% -> 1.0)
        pct_adv = (qty_dec * Decimal("100")) / Decimal(adv)
        impact_bps = (pct_adv * self.impact_bps_per_pct_adv).quantize(
            PRICE_TICK, rounding=ROUND_HALF_UP
        )

        # Adverse price slippage from half-spread + market impact
        slippage_bps = self.half_spread_bps + impact_bps
        slip_frac = slippage_bps / BPS_DIVISOR
        if side.upper() == "BUY":
            fill_price = (reference_price * (Decimal("1") + slip_frac)).quantize(
                PRICE_TICK, rounding=ROUND_HALF_UP
            )
        else:
            fill_price = max(
                Decimal("0.01"),
                (reference_price * (Decimal("1") - slip_frac)).quantize(
                    PRICE_TICK, rounding=ROUND_HALF_UP
                ),
            )

        commission = (notional * (self.commission_bps / BPS_DIVISOR)).quantize(
            CENT, rounding=ROUND_HALF_UP
        )
        spread_cost = (notional * (self.half_spread_bps / BPS_DIVISOR)).quantize(
            CENT, rounding=ROUND_HALF_UP
        )
        impact_cost = (notional * (impact_bps / BPS_DIVISOR)).quantize(CENT, rounding=ROUND_HALF_UP)
        total_cost = (commission + spread_cost + impact_cost).quantize(CENT, rounding=ROUND_HALF_UP)

        return FillCostBreakdown(
            fill_price=fill_price,
            notional=notional,
            commission=commission,
            spread_cost=spread_cost,
            impact_cost=impact_cost,
            total_cost=total_cost,
        )

    def daily_short_borrow_fee(self, short_notional: Decimal) -> Decimal:
        """Daily borrow cost for short notional assuming 252 trading days/year."""
        if short_notional <= Decimal("0") or self.borrow_fee_annual_bps <= Decimal("0"):
            return Decimal("0.00")
        daily_rate = (self.borrow_fee_annual_bps / BPS_DIVISOR) / Decimal("252")
        return (short_notional * daily_rate).quantize(CENT, rounding=ROUND_HALF_UP)
