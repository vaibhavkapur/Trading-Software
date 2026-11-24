"""Allowlisted dataset catalog with point-in-time metadata and checksum verification.

Only explicitly allowlisted, simulated datasets can be accessed by experiments.
Arbitrary external URLs, unapproved local paths, or unentitled datasets are
rejected by the validator and catalog.
"""

from __future__ import annotations

from datetime import UTC, date, datetime

from quant_agent.data.simulator import (
    FaultInjectionConfig,
    compute_dataset_checksum,
    generate_simulated_bars,
)
from quant_agent.models.domain import DatasetVersion, MarketBar

DEFAULT_EQUITY_UNIVERSE = [
    "AAPL_SIM",
    "MSFT_SIM",
    "NVDA_SIM",
    "AMZN_SIM",
    "GOOG_SIM",
    "META_SIM",
    "JPM_SIM",
    "XOM_SIM",
    "UNH_SIM",
    "V_SIM",
    "HD_SIM",
    "PG_SIM",
]

DEFAULT_SECTOR_UNIVERSE = [
    "XLK_SIM",
    "XLF_SIM",
    "XLE_SIM",
    "XLV_SIM",
    "XLI_SIM",
    "XLP_SIM",
]


class DatasetCatalog:
    """Registry of allowlisted, checksum-verified simulated datasets."""

    def __init__(self) -> None:
        self._versions: dict[str, DatasetVersion] = {}
        self._bars_cache: dict[str, dict[str, list[MarketBar]]] = {}
        self._faults: dict[str, FaultInjectionConfig | None] = {}
        self._register_builtin_datasets()

    def _register_builtin_datasets(self) -> None:
        # 1. Primary allowlisted liquid equities dataset (2021-01-04 to 2024-12-31)
        self.register_simulated_dataset(
            dataset_id="sim_us_liquid_equities_v1",
            name="Simulated US Liquid Equities Daily OHLCV v1",
            source="local_deterministic_simulator",
            symbols=DEFAULT_EQUITY_UNIVERSE,
            coverage_start=date(2021, 1, 4),
            coverage_end=date(2024, 12, 31),
            available_asof=datetime(2025, 1, 2, 0, 0, 0, tzinfo=UTC),
            entitlement_tier="simulated_internal",
            is_allowlisted=True,
            seed=20250101,
        )

        # 2. Secondary allowlisted sector ETF dataset
        self.register_simulated_dataset(
            dataset_id="sim_sector_etfs_v1",
            name="Simulated Sector ETFs Daily OHLCV v1",
            source="local_deterministic_simulator",
            symbols=DEFAULT_SECTOR_UNIVERSE,
            coverage_start=date(2021, 1, 4),
            coverage_end=date(2024, 12, 31),
            available_asof=datetime(2025, 1, 2, 0, 0, 0, tzinfo=UTC),
            entitlement_tier="simulated_internal",
            is_allowlisted=True,
            seed=20250202,
        )

        # 3. Restricted-tier dataset (for entitlement boundary testing)
        self.register_simulated_dataset(
            dataset_id="sim_institutional_alt_v1",
            name="Simulated Institutional Alt-Data (Restricted Tier)",
            source="local_deterministic_simulator",
            symbols=DEFAULT_EQUITY_UNIVERSE[:4],
            coverage_start=date(2021, 1, 4),
            coverage_end=date(2024, 12, 31),
            available_asof=datetime(2025, 1, 2, 0, 0, 0, tzinfo=UTC),
            entitlement_tier="restricted_licensed",
            is_allowlisted=True,
            seed=20250303,
        )

    def register_simulated_dataset(
        self,
        *,
        dataset_id: str,
        name: str,
        source: str,
        symbols: list[str],
        coverage_start: date,
        coverage_end: date,
        available_asof: datetime,
        entitlement_tier: str = "simulated_internal",
        is_allowlisted: bool = True,
        seed: int = 42,
        faults: FaultInjectionConfig | None = None,
    ) -> DatasetVersion:
        bars = generate_simulated_bars(
            symbols=symbols,
            start=coverage_start,
            end=coverage_end,
            seed=seed,
            faults=faults,
        )
        checksum = compute_dataset_checksum(bars)
        version = DatasetVersion(
            id=dataset_id,
            name=name,
            source=source,
            checksum=checksum,
            available_asof=available_asof,
            coverage_start=coverage_start,
            coverage_end=coverage_end,
            symbols=sorted(symbols),
            entitlement_tier=entitlement_tier,
            is_allowlisted=is_allowlisted,
            seed=seed,
        )
        self._versions[dataset_id] = version
        self._bars_cache[dataset_id] = bars
        self._faults[dataset_id] = faults
        return version

    def list_datasets(self, include_non_allowlisted: bool = False) -> list[DatasetVersion]:
        versions = list(self._versions.values())
        if not include_non_allowlisted:
            versions = [v for v in versions if v.is_allowlisted]
        return sorted(versions, key=lambda v: v.id)

    def get_version(self, dataset_id: str) -> DatasetVersion | None:
        return self._versions.get(dataset_id)

    def verify_checksum(self, dataset_id: str) -> bool:
        version = self._versions.get(dataset_id)
        bars = self._bars_cache.get(dataset_id)
        if version is None or bars is None:
            return False
        return compute_dataset_checksum(bars) == version.checksum

    def load_bars(
        self,
        dataset_id: str,
        symbols: list[str],
        start: date | None = None,
        end: date | None = None,
    ) -> dict[str, list[MarketBar]]:
        """Load bars for the requested symbols, strictly bounded by [start, end]."""
        version = self._versions.get(dataset_id)
        if version is None:
            raise KeyError(f"Unknown dataset_id: {dataset_id}")
        if not version.is_allowlisted:
            raise PermissionError(f"Dataset {dataset_id!r} is not allowlisted.")

        all_bars = self._bars_cache[dataset_id]
        sliced: dict[str, list[MarketBar]] = {}
        for sym in sorted(symbols):
            if sym not in all_bars:
                raise KeyError(f"Symbol {sym!r} not found in dataset {dataset_id!r}")
            rows = all_bars[sym]
            if start is not None or end is not None:
                rows = [
                    b
                    for b in rows
                    if (start is None or b.trading_date >= start)
                    and (end is None or b.trading_date <= end)
                ]
            sliced[sym] = rows
        return sliced
