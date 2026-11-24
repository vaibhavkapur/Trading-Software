"""Instrument and quote repository with staleness, clock-skew, and disconnect guards."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import UTC, date, datetime
from pathlib import Path

from options_infra.domain.models import (
    ModelWarning,
    OptionContract,
    OptionQuote,
    WarningSeverity,
)
from options_infra.instruments.simulator import (
    DEFAULT_ASOF,
    generate_default_universe,
    load_fixture_file,
)


class InstrumentNotFoundError(LookupError):
    """Raised when a requested option contract is unknown."""


class QuoteNotFoundError(LookupError):
    """Raised when no quote exists for a known contract."""


class QuoteFeedDisconnectedError(RuntimeError):
    """Raised when the simulated market data feed is disconnected."""


class StaleQuoteError(ValueError):
    """Raised when a quote exceeds the maximum allowed staleness threshold."""


class ClockSkewError(ValueError):
    """Raised when venue event time (`asof`) diverges excessively from local receive time."""


@dataclass(frozen=True)
class QuoteIngestionResult:
    accepted: bool
    duplicate: bool
    reason: str
    warnings: tuple[ModelWarning, ...]


class InstrumentRepository:
    """In-memory repository for option contract metadata and latest quotes."""

    def __init__(
        self,
        max_quote_age_seconds: float = 300.0,
        max_clock_skew_seconds: float = 30.0,
    ) -> None:
        self._contracts: dict[str, OptionContract] = {}
        self._quotes: dict[str, OptionQuote] = {}
        self._seen_sequences: dict[str, set[int]] = {}
        self._feed_connected: bool = True
        self.max_quote_age_seconds = max_quote_age_seconds
        self.max_clock_skew_seconds = max_clock_skew_seconds

    @classmethod
    def from_default_simulator(
        cls,
        asof: datetime = DEFAULT_ASOF,
        max_quote_age_seconds: float = 300.0,
        max_clock_skew_seconds: float = 30.0,
    ) -> InstrumentRepository:
        repo = cls(
            max_quote_age_seconds=max_quote_age_seconds,
            max_clock_skew_seconds=max_clock_skew_seconds,
        )
        contracts, quotes = generate_default_universe(asof=asof)
        for contract in contracts:
            repo.upsert_contract(contract)
        for quote in quotes:
            repo.upsert_quote(quote)
        return repo

    @classmethod
    def from_fixture(
        cls,
        path: Path,
        max_quote_age_seconds: float = 300.0,
        max_clock_skew_seconds: float = 30.0,
    ) -> InstrumentRepository:
        repo = cls(
            max_quote_age_seconds=max_quote_age_seconds,
            max_clock_skew_seconds=max_clock_skew_seconds,
        )
        contracts, quotes = load_fixture_file(path)
        for contract in contracts:
            repo.upsert_contract(contract)
        for quote in quotes:
            repo.upsert_quote(quote)
        return repo

    @property
    def feed_connected(self) -> bool:
        return self._feed_connected

    def set_feed_connected(self, connected: bool) -> None:
        """Toggle market-data connectivity (used in failure demos and scenario tests)."""
        self._feed_connected = connected

    def upsert_contract(self, contract: OptionContract) -> None:
        self._contracts[contract.id] = contract

    def upsert_quote(self, quote: OptionQuote) -> QuoteIngestionResult:
        """Ingest a quote with duplicate-sequence protection and clock-skew diagnostics."""
        if not self._feed_connected:
            raise QuoteFeedDisconnectedError(
                "Market data feed is disconnected; refusing quote ingestion"
            )
        if quote.contract_id not in self._contracts:
            raise InstrumentNotFoundError(
                f"Cannot ingest quote for unknown contract {quote.contract_id}"
            )

        seen = self._seen_sequences.setdefault(quote.contract_id, set())
        if quote.sequence_id > 0 and quote.sequence_id in seen:
            return QuoteIngestionResult(
                accepted=False,
                duplicate=True,
                reason=f"Duplicate sequence_id {quote.sequence_id} ignored idempotently",
                warnings=(),
            )

        existing = self._quotes.get(quote.contract_id)
        if existing is not None and quote.asof < existing.asof:
            return QuoteIngestionResult(
                accepted=False,
                duplicate=False,
                reason=(
                    f"Out-of-order quote dropped: venue asof {quote.asof.isoformat()} "
                    f"precedes current {existing.asof.isoformat()}"
                ),
                warnings=(
                    ModelWarning(
                        code="OUT_OF_ORDER_QUOTE_DROPPED",
                        severity=WarningSeverity.WARNING,
                        message="Received older venue timestamp than latest stored quote.",
                    ),
                ),
            )

        warnings: list[ModelWarning] = []
        skew = abs(quote.clock_skew_seconds)
        if skew > self.max_clock_skew_seconds:
            warnings.append(
                ModelWarning(
                    code="CLOCK_SKEW_DETECTED",
                    severity=WarningSeverity.WARNING,
                    message=(
                        f"Venue time ({quote.asof.isoformat()}) vs local receive time "
                        f"({quote.received_at.isoformat()}) skew is "
                        f"{quote.clock_skew_seconds:.2f}s "
                        f"(limit {self.max_clock_skew_seconds:.1f}s)."
                    ),
                )
            )

        self._quotes[quote.contract_id] = quote
        if quote.sequence_id > 0:
            seen.add(quote.sequence_id)
        return QuoteIngestionResult(
            accepted=True,
            duplicate=False,
            reason="Quote accepted",
            warnings=tuple(warnings),
        )

    def get_contract(self, contract_id: str) -> OptionContract:
        try:
            return self._contracts[contract_id]
        except KeyError as exc:
            raise InstrumentNotFoundError(f"Unknown option contract: {contract_id}") from exc

    def get_quote(
        self,
        contract_id: str,
        reference_now: datetime | None = None,
        enforce_freshness: bool = False,
        enforce_clock_skew: bool = False,
    ) -> OptionQuote:
        if not self._feed_connected:
            raise QuoteFeedDisconnectedError(
                f"Market data feed disconnected while fetching quote for {contract_id}"
            )
        if contract_id not in self._contracts:
            raise InstrumentNotFoundError(f"Unknown option contract: {contract_id}")
        quote = self._quotes.get(contract_id)
        if quote is None:
            raise QuoteNotFoundError(f"No quote available for contract: {contract_id}")

        if enforce_clock_skew and abs(quote.clock_skew_seconds) > self.max_clock_skew_seconds:
            raise ClockSkewError(
                f"Clock skew {quote.clock_skew_seconds:.2f}s exceeds limit "
                f"{self.max_clock_skew_seconds:.1f}s for {contract_id}"
            )

        if enforce_freshness:
            now_utc = (reference_now or datetime.now(UTC)).astimezone(UTC)
            age_seconds = (now_utc - quote.asof).total_seconds()
            if age_seconds > self.max_quote_age_seconds:
                raise StaleQuoteError(
                    f"Quote for {contract_id} is stale: age {age_seconds:.1f}s exceeds "
                    f"limit {self.max_quote_age_seconds:.1f}s (asof={quote.asof.isoformat()})"
                )
        return quote

    def inspect_quote_health(
        self,
        quote: OptionQuote,
        reference_now: datetime | None = None,
    ) -> list[ModelWarning]:
        """Return non-fatal health warnings for staleness or clock skew."""
        warnings: list[ModelWarning] = []
        if abs(quote.clock_skew_seconds) > self.max_clock_skew_seconds:
            warnings.append(
                ModelWarning(
                    code="CLOCK_SKEW_DETECTED",
                    severity=WarningSeverity.WARNING,
                    message=(
                        f"Venue/receive clock skew of {quote.clock_skew_seconds:.2f}s exceeds "
                        f"{self.max_clock_skew_seconds:.1f}s threshold."
                    ),
                )
            )
        if reference_now is not None:
            age_seconds = (reference_now.astimezone(UTC) - quote.asof).total_seconds()
            if age_seconds > self.max_quote_age_seconds:
                warnings.append(
                    ModelWarning(
                        code="STALE_QUOTE",
                        severity=WarningSeverity.CRITICAL,
                        message=(
                            f"Quote is stale ({age_seconds:.1f}s old vs "
                            f"{self.max_quote_age_seconds:.1f}s limit)."
                        ),
                    )
                )
        return warnings

    def list_symbols(self) -> list[str]:
        return sorted({c.underlying for c in self._contracts.values()})

    def list_expiries(self, symbol: str) -> list[date]:
        sym = symbol.strip().upper()
        return sorted({c.expiry for c in self._contracts.values() if c.underlying == sym})

    def get_chain(
        self,
        symbol: str,
        expiry: date,
    ) -> list[tuple[OptionContract, OptionQuote | None]]:
        if not self._feed_connected:
            raise QuoteFeedDisconnectedError(
                f"Market data feed disconnected while fetching chain for {symbol} {expiry}"
            )
        sym = symbol.strip().upper()
        matches = [
            c
            for c in self._contracts.values()
            if c.underlying == sym and c.expiry == expiry
        ]
        matches.sort(key=lambda c: (c.strike, c.right.value))
        return [(c, self._quotes.get(c.id)) for c in matches]

    def latest_quote_time(self) -> datetime | None:
        if not self._quotes:
            return None
        return max(q.asof for q in self._quotes.values())
