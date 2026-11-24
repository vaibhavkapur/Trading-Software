"""Constrained Hypothesis & Experiment Planner.

Translates a natural-language research question into:
1. A falsifiable `Hypothesis` with an explicit null hypothesis and allowlisted
   `SignalDefinition` (no arbitrary code).
2. A leakage-safe initial `Experiment` plan with separated train/val/test dates,
   purge/embargo buffers, and explicit transaction cost assumptions.

Guardrail: The planner NEVER asserts profitability or fabricates performance numbers.
"""

from __future__ import annotations

from datetime import date
from decimal import Decimal
from typing import Protocol

from quant_agent.data.catalog import DatasetCatalog
from quant_agent.models.domain import DateRange, Experiment, Hypothesis
from quant_agent.models.signals import (
    DirectionMode,
    RebalanceFrequency,
    SignalDefinition,
    SignalType,
    TransactionCostAssumptions,
    WalkForwardConfig,
)


class PlannerBackend(Protocol):
    """Pluggable interface for optional LLM-backed constrained schema planning."""

    def propose(
        self,
        question: str,
        *,
        dataset_id: str | None = None,
        universe: list[str] | None = None,
        horizon_days: int | None = None,
    ) -> tuple[Hypothesis, Experiment]: ...


class ResearchPlanner:
    """Deterministic constrained planner mapping research questions to structured schemas."""

    def __init__(
        self,
        catalog: DatasetCatalog,
        *,
        planner_version: str = "1.0.0",
    ) -> None:
        self.catalog = catalog
        self.planner_version = planner_version

    def propose(
        self,
        question: str,
        *,
        dataset_id: str | None = None,
        universe: list[str] | None = None,
        horizon_days: int | None = None,
        walk_forward: bool = False,
        seed: int = 20260101,
    ) -> tuple[Hypothesis, Experiment]:
        q_lower = question.lower().strip()

        # Select allowlisted dataset
        chosen_dataset_id = dataset_id or (
            "sim_sector_etfs_v1"
            if any(k in q_lower for k in ("sector", "etf", "rotation"))
            else "sim_us_liquid_equities_v1"
        )
        ds = self.catalog.get_version(chosen_dataset_id)
        if ds is None or not ds.is_allowlisted:
            ds = self.catalog.get_version("sim_us_liquid_equities_v1")
            assert ds is not None
            chosen_dataset_id = ds.id

        chosen_universe = list(universe) if universe else list(ds.symbols[:8])
        signal_type, default_horizon, lookback, short_w, z_thresh, rationale = (
            self._classify_question(q_lower)
        )
        eff_horizon = horizon_days if horizon_days is not None else default_horizon

        signal_def = SignalDefinition(
            signal_type=signal_type,
            lookback_days=lookback,
            short_window=short_w,
            zscore_threshold=z_thresh,
            lag_bars=1,
            direction=DirectionMode.LONG_SHORT,
            rebalance=RebalanceFrequency.DAILY if eff_horizon <= 5 else RebalanceFrequency.WEEKLY,
            max_positions=min(6, max(2, len(chosen_universe))),
            feature_shift=0,
        )

        statement = (
            f"In simulated universe ({len(chosen_universe)} symbols from {chosen_dataset_id}), "
            f"a strictly causal {signal_type.value} signal (lookback={lookback}d, lag=1 bar) "
            f"predicts net-of-cost {eff_horizon}-day forward returns."
        )
        null_hypothesis = (
            f"H0: After transaction costs and next-open execution lag, the {signal_type.value} "
            f"signal yields zero expected {eff_horizon}-day out-of-sample excess return "
            "(mean daily return <= 0 or indistinguishable from noise under block bootstrap)."
        )

        hypothesis = Hypothesis(
            question=question.strip(),
            statement=statement,
            null_hypothesis=null_hypothesis,
            universe=chosen_universe,
            signal_def=signal_def,
            horizon_days=eff_horizon,
            dataset_id=chosen_dataset_id,
            economic_rationale=rationale,
            planner_version=self.planner_version,
        )

        # Construct strictly chronological, purged train / val / test windows within coverage
        purge_days = max(5, eff_horizon + 2)
        embargo_days = 2
        experiment = Experiment(
            hypothesis_id=hypothesis.id,
            dataset_version_id=chosen_dataset_id,
            universe=chosen_universe,
            signal_def=signal_def,
            horizon_days=eff_horizon,
            train_range=DateRange(start=date(2021, 1, 4), end=date(2022, 12, 30)),
            val_range=DateRange(start=date(2023, 1, 16), end=date(2023, 12, 29)),
            test_range=DateRange(start=date(2024, 1, 15), end=date(2024, 12, 31)),
            purge_days=purge_days,
            embargo_days=embargo_days,
            initial_capital=Decimal("1000000.00"),
            cost_assumptions=TransactionCostAssumptions(
                commission_bps=1.5,
                half_spread_bps=2.0,
                impact_bps_per_pct_adv=10.0,
                borrow_fee_annual_bps=50.0,
            ),
            walk_forward=WalkForwardConfig(enabled=walk_forward, num_folds=3),
            seed=seed,
        )
        experiment.config_hash = experiment.compute_config_hash()
        return hypothesis, experiment

    def _classify_question(self, q_lower: str) -> tuple[SignalType, int, int, int, float, str]:
        if "volume" in q_lower and any(
            k in q_lower for k in ("momentum", "continuation", "follow")
        ):
            return (
                SignalType.ABNORMAL_VOLUME_MOMENTUM,
                2,
                20,
                5,
                1.5,
                "Tests whether abnormal volume spikes accompany informed order flow that persists.",
            )
        if "volume" in q_lower:
            return (
                SignalType.ABNORMAL_VOLUME_REVERSAL,
                1,
                20,
                5,
                1.5,
                "Tests whether abnormal volume spikes reflect temporary liquidity pressure "
                "that reverts.",
            )
        if any(k in q_lower for k in ("mean reversion", "revert", "oversold", "zscore", "z-score")):
            return (
                SignalType.MEAN_REVERSION_ZSCORE,
                3,
                20,
                5,
                1.5,
                "Tests whether short-horizon price dislocations revert toward their trailing mean.",
            )
        if any(k in q_lower for k in ("breakout", "volatility", "vol")):
            return (
                SignalType.VOLATILITY_BREAKOUT,
                5,
                20,
                5,
                1.5,
                "Tests whether large volatility-adjusted moves continue over a multi-day horizon.",
            )
        if any(k in q_lower for k in ("moving average", "crossover", "sma", "ma ")):
            return (
                SignalType.MOVING_AVERAGE_CROSSOVER,
                5,
                40,
                10,
                1.0,
                "Tests whether short-vs-long moving-average trend alignment predicts "
                "forward returns.",
            )
        return (
            SignalType.CROSS_SECTIONAL_MOMENTUM,
            5,
            60,
            5,
            1.0,
            "Tests whether cross-sectional trailing winners outperform losers over "
            "a weekly horizon.",
        )
