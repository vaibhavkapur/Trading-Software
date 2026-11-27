"""Market data entitlement, staleness, clock skew, and sequence validation guard."""

from __future__ import annotations

from datetime import datetime
from decimal import Decimal

from trading_platform.domain.config import FeedConfig
from trading_platform.domain.errors import (
    ClockSkewError,
    DataEntitlementError,
    StaleFeedError,
)
from trading_platform.domain.events import ExecutionMode, FeedEntitlement, MarketTick, utc_now
from trading_platform.observability.telemetry import TelemetryRecorder


class MarketDataGuard:
    """Validates entitlements, UTC clock skew, feed freshness, and point-in-time ordering."""

    def __init__(
        self,
        *,
        config: FeedConfig,
        execution_mode: ExecutionMode,
        telemetry: TelemetryRecorder,
    ) -> None:
        self.config = config
        self.execution_mode = execution_mode
        self.telemetry = telemetry
        self._entitlements: dict[str, FeedEntitlement] = {}
        self._last_sequence: dict[tuple[str, str], int] = {}
        self._last_tick: dict[str, MarketTick] = {}

    def register_entitlement(self, entitlement: FeedEntitlement) -> None:
        self._entitlements[entitlement.feed_id] = entitlement

    def latest_tick(self, symbol: str) -> MarketTick | None:
        return self._last_tick.get(symbol)

    def latest_marks(self) -> dict[str, Decimal]:
        return {sym: tick.mid for sym, tick in self._last_tick.items()}

    def validate_tick(
        self,
        tick: MarketTick,
        *,
        now_utc: datetime | None = None,
    ) -> MarketTick:
        now = now_utc or utc_now()
        self.telemetry.inc("market_ticks_received")

        # 1. Entitlement check
        if self.config.require_entitlement:
            ent = self._entitlements.get(tick.feed_id)
            if ent is None:
                self.telemetry.inc("market_ticks_entitlement_rejected")
                raise DataEntitlementError(
                    f"Feed {tick.feed_id} has no registered data entitlement",
                    details={"feed_id": tick.feed_id, "symbol": tick.symbol},
                )
            if self.execution_mode not in ent.allowed_modes:
                self.telemetry.inc("market_ticks_entitlement_rejected")
                raise DataEntitlementError(
                    f"Feed {tick.feed_id} is not entitled for mode {self.execution_mode.value}",
                    details={
                        "feed_id": tick.feed_id,
                        "mode": self.execution_mode.value,
                        "allowed_modes": [m.value for m in ent.allowed_modes],
                    },
                )
            if tick.symbol not in ent.symbols:
                self.telemetry.inc("market_ticks_entitlement_rejected")
                raise DataEntitlementError(
                    f"Symbol {tick.symbol} not entitled on feed {tick.feed_id}",
                    details={"feed_id": tick.feed_id, "symbol": tick.symbol},
                )
            if now > ent.valid_until_utc:
                self.telemetry.inc("market_ticks_entitlement_rejected")
                raise DataEntitlementError(
                    f"Entitlement for feed {tick.feed_id} expired at {ent.valid_until_utc.isoformat()}",
                    details={"feed_id": tick.feed_id},
                )

        # 2. Clock skew guard:
        #    a) available_time_utc cannot precede event_time_utc (point-in-time causality)
        #    b) event_time_utc cannot be ahead of reference wall clock beyond max_clock_skew_ms
        #    c) |available_time_utc - event_time_utc| cannot show negative/impossible skew
        if tick.available_time_utc < tick.event_time_utc:
            skew_ms = (tick.event_time_utc - tick.available_time_utc).total_seconds() * 1000.0
            self.telemetry.inc("market_ticks_clock_skew")
            raise ClockSkewError(
                f"Point-in-time clock skew on {tick.symbol}: event_time exceeds available_time by {skew_ms:.1f}ms",
                details={"tick_id": tick.tick_id, "skew_ms": round(skew_ms, 2)},
            )

        future_skew_ms = (tick.event_time_utc - now).total_seconds() * 1000.0
        if future_skew_ms > self.config.max_clock_skew_ms:
            self.telemetry.inc("market_ticks_clock_skew")
            raise ClockSkewError(
                f"Forward clock skew detected on {tick.symbol}: event_time is {future_skew_ms:.1f}ms ahead of UTC clock (max {self.config.max_clock_skew_ms}ms)",
                details={
                    "tick_id": tick.tick_id,
                    "future_skew_ms": round(future_skew_ms, 2),
                    "max_clock_skew_ms": self.config.max_clock_skew_ms,
                },
            )

        avail_future_ms = (tick.available_time_utc - now).total_seconds() * 1000.0
        if avail_future_ms > self.config.max_clock_skew_ms:
            self.telemetry.inc("market_ticks_clock_skew")
            raise ClockSkewError(
                f"Forward available_time clock skew on {tick.symbol}: {avail_future_ms:.1f}ms ahead of UTC clock",
                details={"tick_id": tick.tick_id, "avail_future_ms": round(avail_future_ms, 2)},
            )

        # 3. Stale feed guard
        staleness_ms = (now - tick.available_time_utc).total_seconds() * 1000.0
        event_age_ms = (now - tick.event_time_utc).total_seconds() * 1000.0
        effective_staleness = max(staleness_ms, event_age_ms)
        self.telemetry.set_gauge(f"feed_staleness_ms.{tick.symbol}", effective_staleness)
        if effective_staleness > self.config.max_staleness_ms:
            self.telemetry.inc("market_ticks_stale")
            raise StaleFeedError(
                f"Stale feed detected for {tick.symbol}: age {effective_staleness:.1f}ms exceeds max {self.config.max_staleness_ms}ms",
                details={
                    "tick_id": tick.tick_id,
                    "symbol": tick.symbol,
                    "staleness_ms": round(effective_staleness, 2),
                    "max_staleness_ms": self.config.max_staleness_ms,
                },
            )

        # 4. Out-of-order sequence guard
        seq_key = (tick.feed_id, tick.symbol)
        prev_seq = self._last_sequence.get(seq_key)
        if prev_seq is not None and tick.sequence_no <= prev_seq:
            self.telemetry.inc("market_ticks_out_of_order")
            raise StaleFeedError(
                f"Non-monotonic sequence for {tick.symbol}: got {tick.sequence_no} <= {prev_seq}",
                details={"tick_id": tick.tick_id, "sequence_no": tick.sequence_no, "prev_seq": prev_seq},
            )

        self._last_sequence[seq_key] = tick.sequence_no
        self._last_tick[tick.symbol] = tick
        self.telemetry.inc("market_ticks_accepted")
        return tick
