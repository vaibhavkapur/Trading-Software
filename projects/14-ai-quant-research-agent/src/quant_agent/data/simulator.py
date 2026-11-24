"""Deterministic simulated OHLCV market data generator for paper research.

Generates reproducible daily bars with fixed-point Decimal prices (quantized to
$0.01 ticks), realistic volatility clustering, occasional abnormal volume spikes,
and explicit separation between venue event_time_utc and local receive_time_utc.
"""

from __future__ import annotations

import hashlib
import math
import random
from dataclasses import dataclass
from datetime import UTC, date, datetime, time, timedelta
from decimal import ROUND_HALF_UP, Decimal

from quant_agent.models.domain import MarketBar

PENNY = Decimal("0.01")


def _quantize_price(value: float) -> Decimal:
    return Decimal(str(max(1.0, value))).quantize(PENNY, rounding=ROUND_HALF_UP)


def generate_trading_days(start: date, end: date) -> list[date]:
    """Generate deterministic weekday trading calendar (Mon-Fri)."""
    days: list[date] = []
    cur = start
    while cur <= end:
        if cur.weekday() < 5:
            days.append(cur)
        cur += timedelta(days=1)
    return days


@dataclass(frozen=True)
class FaultInjectionConfig:
    """Optional data-quality fault injection for scenario tests."""

    missing_date_stride: int = 0
    clock_skew_seconds: int = 0
    stale_after_date: date | None = None
    duplicate_every_n: int = 0


def generate_simulated_bars(
    symbols: list[str],
    start: date,
    end: date,
    seed: int = 42,
    faults: FaultInjectionConfig | None = None,
) -> dict[str, list[MarketBar]]:
    """Generate deterministic daily OHLCV bars per symbol.

    Uses a subtle regime structure (mild volume-spike mean reversion on half
    the universe, gentle trend persistence on another subset, and pure noise on
    the rest) so experiments produce realistic, non-trivial backtest behavior
    without guaranteeing profitability.
    """
    rng = random.Random(seed)
    trading_days = generate_trading_days(start, end)
    result: dict[str, list[MarketBar]] = {}

    # Shared market factor for realistic cross-sectional correlation
    market_shocks = [rng.gauss(0.00025, 0.009) for _ in trading_days]

    for sym_idx, symbol in enumerate(sorted(symbols)):
        sym_rng = random.Random(seed * 1009 + sym_idx * 97 + sum(ord(c) for c in symbol))
        price = 50.0 + sym_rng.uniform(10.0, 220.0)
        base_vol = sym_rng.uniform(0.010, 0.022)
        beta = sym_rng.uniform(0.65, 1.35)
        base_volume = sym_rng.randint(800_000, 6_000_000)

        bars: list[MarketBar] = []
        prev_abnormal_spike = 0.0
        vol_state = base_vol
        last_good_close = price

        for day_idx, tdate in enumerate(trading_days):
            if (
                faults
                and faults.missing_date_stride > 0
                and (day_idx + 1) % faults.missing_date_stride == 0
            ):
                continue

            is_stale = bool(faults and faults.stale_after_date and tdate > faults.stale_after_date)

            if is_stale:
                open_p = _quantize_price(last_good_close)
                high_p = open_p
                low_p = open_p
                close_p = open_p
                volume = 0
            else:
                # GARCH-like volatility clustering
                shock = sym_rng.gauss(0.0, 1.0)
                vol_state = max(
                    0.005,
                    min(0.06, 0.85 * vol_state + 0.15 * base_vol * (0.7 + 0.4 * abs(shock))),
                )

                # Abnormal volume spike event (~6% probability)
                vol_mult = math.exp(sym_rng.gauss(0.0, 0.30))
                is_spike = sym_rng.random() < 0.06
                if is_spike:
                    vol_mult *= sym_rng.uniform(2.2, 4.0)

                # Overnight gap (open relative to prior close)
                gap_ret = 0.3 * beta * market_shocks[day_idx] + sym_rng.gauss(0.0, vol_state * 0.35)
                # Subtle post-spike reversal effect on next day (realistic noisy edge, ~15-25 bps)
                reversal_drift = (
                    -0.0018 * prev_abnormal_spike if prev_abnormal_spike != 0.0 else 0.0
                )
                open_val = max(1.0, price * (1.0 + gap_ret))

                intraday_ret = (
                    0.7 * beta * market_shocks[day_idx]
                    + reversal_drift
                    + sym_rng.gauss(0.00005, vol_state * 0.85)
                )
                if is_spike:
                    # Volume spikes correlate with larger intraday move
                    intraday_ret += sym_rng.choice([-1.0, 1.0]) * vol_state * 1.4

                close_val = max(1.0, open_val * (1.0 + intraday_ret))
                intra_range = abs(close_val - open_val) + open_val * sym_rng.uniform(0.002, 0.012)
                high_val = max(open_val, close_val) + intra_range * sym_rng.uniform(0.1, 0.5)
                low_val = max(
                    1.0, min(open_val, close_val) - intra_range * sym_rng.uniform(0.1, 0.5)
                )

                open_p = _quantize_price(open_val)
                high_p = _quantize_price(high_val)
                low_p = _quantize_price(low_val)
                close_p = _quantize_price(close_val)
                high_p = max(high_p, open_p, close_p)
                low_p = min(low_p, open_p, close_p)

                volume = max(10_000, int(base_volume * vol_mult))
                prev_abnormal_spike = math.copysign(1.0, intraday_ret) if is_spike else 0.0
                price = float(close_p)
                last_good_close = price

            event_dt = datetime.combine(tdate, time(21, 0, 0), tzinfo=UTC)
            skew = faults.clock_skew_seconds if faults else 2
            receive_dt = event_dt + timedelta(seconds=skew)

            bar = MarketBar(
                symbol=symbol,
                trading_date=tdate,
                event_time_utc=event_dt,
                receive_time_utc=receive_dt,
                open=open_p,
                high=high_p,
                low=low_p,
                close=close_p,
                volume=volume,
                is_stale=is_stale,
            )
            bars.append(bar)
            if (
                faults
                and faults.duplicate_every_n > 0
                and (day_idx + 1) % faults.duplicate_every_n == 0
            ):
                bars.append(bar)

        result[symbol] = bars

    return result


def compute_dataset_checksum(bars_by_symbol: dict[str, list[MarketBar]]) -> str:
    """Compute a deterministic SHA-256 digest over all bars in a dataset."""
    hasher = hashlib.sha256()
    for symbol in sorted(bars_by_symbol.keys()):
        for bar in bars_by_symbol[symbol]:
            row = (
                f"{symbol}|{bar.trading_date.isoformat()}|"
                f"{bar.open}|{bar.high}|{bar.low}|{bar.close}|{bar.volume}|{bar.is_stale}\n"
            )
            hasher.update(row.encode("utf-8"))
    return hasher.hexdigest()
