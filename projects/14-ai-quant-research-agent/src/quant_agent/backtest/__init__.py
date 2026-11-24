"""Event-driven backtester, transaction cost model, and statistical tests."""

from quant_agent.backtest.costs import FillCostBreakdown, TransactionCostModel
from quant_agent.backtest.engine import BacktestExecutionOutput, EventDrivenBacktester
from quant_agent.backtest.stats import (
    BootstrapCI,
    compute_deflated_sharpe_probability,
    compute_is_vs_oos_degradation,
    compute_max_drawdown,
    compute_sharpe,
    compute_sortino,
    compute_t_stat_and_pvalue,
    evaluate_multiple_testing,
    stationary_block_bootstrap,
)

__all__ = [
    "BacktestExecutionOutput",
    "BootstrapCI",
    "EventDrivenBacktester",
    "FillCostBreakdown",
    "TransactionCostModel",
    "compute_deflated_sharpe_probability",
    "compute_is_vs_oos_degradation",
    "compute_max_drawdown",
    "compute_sharpe",
    "compute_sortino",
    "compute_t_stat_and_pvalue",
    "evaluate_multiple_testing",
    "stationary_block_bootstrap",
]
