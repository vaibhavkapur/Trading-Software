"""Event-driven backtesting engine. Simulation only: nothing here can place a real order."""

from ._version import FILL_MODEL_VERSION, __version__
from .config import BacktestConfig, ConfigError, ExecutionConfig, RiskLimits
from .data import DataError, Dataset, load_dataset
from .engine import Backtest, LookaheadError, run_backtest
from .events import ActionType, Bar, CorporateAction, EventType, MarketEvent, Quote, Timer
from .fixedpoint import fmt_units, to_units
from .metrics import Metrics
from .orders import (
    Liquidity,
    OrderStatus,
    OrderType,
    OrderView,
    Side,
    SimFill,
    SimOrder,
    TimeInForce,
)
from .portfolio import LedgerEntry, LedgerKind, Portfolio, PortfolioSnapshot, ReconciliationError
from .results import BacktestResult, BacktestRun
from .strategy import Context, Strategy
from .timeutil import fmt_ts, parse_ts
from .walkforward import WalkForwardConfig, WalkForwardResult, walk_forward

__all__ = [
    "FILL_MODEL_VERSION",
    "ActionType",
    "Backtest",
    "BacktestConfig",
    "BacktestResult",
    "BacktestRun",
    "Bar",
    "ConfigError",
    "Context",
    "CorporateAction",
    "DataError",
    "Dataset",
    "EventType",
    "ExecutionConfig",
    "LedgerEntry",
    "LedgerKind",
    "Liquidity",
    "LookaheadError",
    "MarketEvent",
    "Metrics",
    "OrderStatus",
    "OrderType",
    "OrderView",
    "Portfolio",
    "PortfolioSnapshot",
    "Quote",
    "ReconciliationError",
    "RiskLimits",
    "Side",
    "SimFill",
    "SimOrder",
    "Strategy",
    "TimeInForce",
    "Timer",
    "WalkForwardConfig",
    "WalkForwardResult",
    "__version__",
    "fmt_ts",
    "fmt_units",
    "load_dataset",
    "parse_ts",
    "run_backtest",
    "to_units",
    "walk_forward",
]
