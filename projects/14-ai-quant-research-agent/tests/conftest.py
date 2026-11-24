"""Shared pytest fixtures for unit, integration, and scenario tests."""

from __future__ import annotations

from datetime import UTC, date, datetime
from decimal import Decimal
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from quant_agent.api.app import ResearchApplicationState, create_app
from quant_agent.config import Settings
from quant_agent.data.catalog import DEFAULT_EQUITY_UNIVERSE, DatasetCatalog
from quant_agent.models.domain import DateRange, Experiment, Hypothesis
from quant_agent.models.signals import (
    DirectionMode,
    RebalanceFrequency,
    SignalDefinition,
    SignalType,
    TransactionCostAssumptions,
)


@pytest.fixture
def tmp_settings(tmp_path: Path) -> Settings:
    return Settings(
        data_dir=tmp_path / "data",
        mode="PAPER_SIMULATED",
        code_revision="test-rev-001",
        max_workers=2,
        require_auth=False,
    )


@pytest.fixture
def catalog() -> DatasetCatalog:
    return DatasetCatalog()


@pytest.fixture
def app_state(tmp_settings: Settings) -> ResearchApplicationState:
    st = ResearchApplicationState(settings=tmp_settings)
    yield st
    st.close()


@pytest.fixture
def client(tmp_settings: Settings) -> TestClient:
    app = create_app(settings=tmp_settings)
    with TestClient(app) as c:
        yield c


def make_valid_hypothesis(
    *,
    signal_type: SignalType = SignalType.ABNORMAL_VOLUME_REVERSAL,
    universe: list[str] | None = None,
    horizon_days: int = 1,
    dataset_id: str = "sim_us_liquid_equities_v1",
) -> Hypothesis:
    return Hypothesis(
        question="Does abnormal volume predict next-day returns in a liquid equity universe?",
        statement="Abnormal volume spikes revert over the next trading session.",
        null_hypothesis="H0: Net-of-cost next-day excess return following volume spikes is <= 0.",
        universe=universe or DEFAULT_EQUITY_UNIVERSE[:6],
        signal_def=SignalDefinition(
            signal_type=signal_type,
            lookback_days=20,
            short_window=5,
            zscore_threshold=1.5,
            lag_bars=1,
            direction=DirectionMode.LONG_SHORT,
            rebalance=RebalanceFrequency.DAILY,
            max_positions=4,
        ),
        horizon_days=horizon_days,
        dataset_id=dataset_id,
    )


def make_valid_experiment(
    hypothesis: Hypothesis,
    *,
    seed: int = 20260101,
    purge_days: int = 5,
    dataset_version_id: str | None = None,
) -> Experiment:
    exp = Experiment(
        hypothesis_id=hypothesis.id,
        dataset_version_id=dataset_version_id or hypothesis.dataset_id,
        universe=list(hypothesis.universe),
        signal_def=hypothesis.signal_def,
        horizon_days=hypothesis.horizon_days,
        train_range=DateRange(start=date(2021, 1, 4), end=date(2022, 12, 30)),
        val_range=DateRange(start=date(2023, 1, 16), end=date(2023, 12, 29)),
        test_range=DateRange(start=date(2024, 1, 15), end=date(2024, 12, 31)),
        purge_days=purge_days,
        embargo_days=2,
        initial_capital=Decimal("1000000.00"),
        cost_assumptions=TransactionCostAssumptions(
            commission_bps=1.5,
            half_spread_bps=2.0,
            impact_bps_per_pct_adv=10.0,
        ),
        seed=seed,
        asof_timestamp_utc=datetime(2025, 6, 1, 0, 0, 0, tzinfo=UTC),
    )
    exp.config_hash = exp.compute_config_hash()
    return exp
