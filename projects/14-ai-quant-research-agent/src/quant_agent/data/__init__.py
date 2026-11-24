"""Data catalog and deterministic market simulator."""

from quant_agent.data.catalog import (
    DEFAULT_EQUITY_UNIVERSE,
    DEFAULT_SECTOR_UNIVERSE,
    DatasetCatalog,
)
from quant_agent.data.simulator import (
    FaultInjectionConfig,
    compute_dataset_checksum,
    generate_simulated_bars,
    generate_trading_days,
)

__all__ = [
    "DEFAULT_EQUITY_UNIVERSE",
    "DEFAULT_SECTOR_UNIVERSE",
    "DatasetCatalog",
    "FaultInjectionConfig",
    "compute_dataset_checksum",
    "generate_simulated_bars",
    "generate_trading_days",
]
