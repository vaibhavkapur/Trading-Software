"""Deterministic option chain simulator and replay fixture loader.

Generates arbitrage-free European (XYZ) and American (ABC) simulated option chains
with realistic volatility skew and tick-quantized bid/ask spreads. All data is
explicitly labeled as simulated (`SIMULATED_FEED`).
"""

from __future__ import annotations

import json
import math
from datetime import UTC, date, datetime, timedelta
from decimal import ROUND_HALF_UP, Decimal
from pathlib import Path

from options_infra.domain.models import (
    ExerciseStyle,
    OptionContract,
    OptionQuote,
    OptionRight,
)
from options_infra.pricing.black_scholes import BSMInput, price_bsm, time_to_expiry_years

DEFAULT_ASOF = datetime(2026, 10, 17, 15, 30, 0, tzinfo=UTC)


def _quantize_tick(value: float, tick: Decimal) -> Decimal:
    raw = Decimal(str(max(0.0, value)))
    return raw.quantize(tick, rounding=ROUND_HALF_UP)


def contract_id_for(underlying: str, expiry: date, strike: Decimal, right: OptionRight) -> str:
    strike_str = (
        str(int(strike))
        if strike == strike.to_integral_value()
        else str(strike.normalize()).replace(".", "p")
    )
    suffix = "C" if right == OptionRight.CALL else "P"
    return f"{underlying.upper()}-{expiry.strftime('%Y%m%d')}-{strike_str}-{suffix}"


def _smile_vol(spot: float, strike: float, base_vol: float, skew: float, convexity: float) -> float:
    """Deterministic quadratic volatility smile in log-moneyness."""
    m = math.log(strike / spot)
    return max(0.08, min(1.50, base_vol + skew * m + convexity * m * m))


def generate_simulated_chain(
    underlying: str = "XYZ",
    spot: Decimal = Decimal("100.00"),
    expiry: date = date(2027, 1, 15),
    strikes: list[Decimal] | None = None,
    rate: Decimal = Decimal("0.04"),
    dividend_yield: Decimal = Decimal("0.00"),
    base_vol: float = 0.28,
    skew: float = -0.18,
    convexity: float = 0.35,
    style: ExerciseStyle = ExerciseStyle.EUROPEAN,
    multiplier: int = 100,
    tick_size: Decimal = Decimal("0.01"),
    asof: datetime = DEFAULT_ASOF,
    receive_latency_ms: int = 45,
    sequence_start: int = 1000,
) -> tuple[list[OptionContract], list[OptionQuote]]:
    """Build deterministic contracts and quotes for an underlying and expiry."""
    if strikes is None:
        strikes = [
            Decimal("85.00"),
            Decimal("90.00"),
            Decimal("95.00"),
            Decimal("100.00"),
            Decimal("105.00"),
            Decimal("110.00"),
            Decimal("115.00"),
        ]

    asof_utc = asof.astimezone(UTC)
    received_at = asof_utc + timedelta(milliseconds=receive_latency_ms)
    years = time_to_expiry_years(expiry=expiry, asof=asof_utc)

    contracts: list[OptionContract] = []
    quotes: list[OptionQuote] = []
    seq = sequence_start

    for strike in strikes:
        vol = _smile_vol(float(spot), float(strike), base_vol, skew, convexity)
        for right in (OptionRight.CALL, OptionRight.PUT):
            cid = contract_id_for(underlying, expiry, strike, right)
            contract = OptionContract(
                id=cid,
                underlying=underlying,
                expiry=expiry,
                strike=strike.quantize(tick_size),
                right=right,
                multiplier=multiplier,
                style=style,
                tick_size=tick_size,
            )
            res = price_bsm(
                BSMInput(
                    right=right,
                    spot=float(spot),
                    strike=float(strike),
                    years=years,
                    rate=float(rate),
                    dividend_yield=float(dividend_yield),
                    volatility=vol,
                    style=style,
                )
            )
            half_spread = max(float(tick_size), min(0.15, res.price * 0.012))
            bid = _quantize_tick(max(0.01, res.price - half_spread), tick_size)
            ask = _quantize_tick(max(float(bid) + float(tick_size), res.price + half_spread), tick_size)

            quote = OptionQuote(
                contract_id=cid,
                bid=bid,
                ask=ask,
                underlying_px=spot.quantize(tick_size),
                asof=asof_utc,
                received_at=received_at,
                rate=rate,
                dividend_yield=dividend_yield,
                source="SIMULATED_FEED",
                sequence_id=seq,
            )
            contracts.append(contract)
            quotes.append(quote)
            seq += 1

    return contracts, quotes


def generate_default_universe(
    asof: datetime = DEFAULT_ASOF,
) -> tuple[list[OptionContract], list[OptionQuote]]:
    """Generate default multi-expiry European XYZ and American ABC chains."""
    all_contracts: list[OptionContract] = []
    all_quotes: list[OptionQuote] = []

    c1, q1 = generate_simulated_chain(
        underlying="XYZ",
        spot=Decimal("100.00"),
        expiry=date(2027, 1, 15),
        rate=Decimal("0.04"),
        dividend_yield=Decimal("0.00"),
        base_vol=0.28,
        style=ExerciseStyle.EUROPEAN,
        asof=asof,
        sequence_start=1000,
    )
    c2, q2 = generate_simulated_chain(
        underlying="XYZ",
        spot=Decimal("100.00"),
        expiry=date(2027, 3, 19),
        rate=Decimal("0.04"),
        dividend_yield=Decimal("0.00"),
        base_vol=0.30,
        style=ExerciseStyle.EUROPEAN,
        asof=asof,
        sequence_start=2000,
    )
    c3, q3 = generate_simulated_chain(
        underlying="ABC",
        spot=Decimal("150.00"),
        expiry=date(2027, 1, 15),
        strikes=[
            Decimal("135.00"),
            Decimal("140.00"),
            Decimal("145.00"),
            Decimal("150.00"),
            Decimal("155.00"),
            Decimal("160.00"),
            Decimal("165.00"),
        ],
        rate=Decimal("0.045"),
        dividend_yield=Decimal("0.02"),
        base_vol=0.32,
        style=ExerciseStyle.AMERICAN,
        asof=asof,
        sequence_start=3000,
    )

    for c, q in ((c1, q1), (c2, q2), (c3, q3)):
        all_contracts.extend(c)
        all_quotes.extend(q)
    return all_contracts, all_quotes


def save_fixture_file(
    path: Path,
    contracts: list[OptionContract],
    quotes: list[OptionQuote],
    metadata: dict[str, str] | None = None,
) -> None:
    """Persist deterministic contracts and quotes fixture to JSON."""
    payload = {
        "schema_version": "1.0.0",
        "metadata": metadata or {"source": "SIMULATED_FEED", "note": "Deterministic replay fixture"},
        "contracts": [c.model_dump(mode="json") for c in contracts],
        "quotes": [q.model_dump(mode="json") for q in quotes],
    }
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")


def load_fixture_file(path: Path) -> tuple[list[OptionContract], list[OptionQuote]]:
    """Load deterministic contracts and quotes from a JSON replay fixture."""
    raw = json.loads(path.read_text(encoding="utf-8"))
    contracts = [OptionContract.model_validate(item) for item in raw["contracts"]]
    quotes = [OptionQuote.model_validate(item) for item in raw["quotes"]]
    return contracts, quotes
