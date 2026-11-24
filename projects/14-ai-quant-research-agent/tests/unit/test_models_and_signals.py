"""Unit tests for domain models, UTC enforcement, config hashes, and safety boundaries."""

from __future__ import annotations

from datetime import UTC, datetime

import pytest
from pydantic import ValidationError

from quant_agent.config import Settings
from quant_agent.models.domain import MarketBar
from quant_agent.models.signals import SignalDefinition, SignalType
from tests.conftest import make_valid_experiment, make_valid_hypothesis


def test_settings_rejects_live_mode():
    with pytest.raises(ValueError, match="PAPER_SIMULATED"):
        Settings(mode="LIVE_TRADING")


def test_market_bar_rejects_naive_timestamps():
    with pytest.raises(ValidationError, match="timezone-aware UTC"):
        MarketBar(
            symbol="AAPL_SIM",
            trading_date=datetime(2024, 1, 2).date(),
            event_time_utc=datetime(2024, 1, 2, 21, 0, 0),  # naive!
            receive_time_utc=datetime(2024, 1, 2, 21, 0, 2, tzinfo=UTC),
            open="100.00",
            high="101.00",
            low="99.00",
            close="100.50",
            volume=100000,
        )


def test_config_hash_is_deterministic_and_sensitive_to_seed():
    hyp = make_valid_hypothesis()
    e1 = make_valid_experiment(hyp, seed=100)
    e2 = make_valid_experiment(hyp, seed=100)
    e3 = make_valid_experiment(hyp, seed=101)

    assert e1.compute_config_hash() == e2.compute_config_hash()
    assert e1.compute_config_hash() != e3.compute_config_hash()


def test_signal_definition_rejects_zero_lag_and_extra_fields():
    with pytest.raises(ValidationError):
        SignalDefinition(signal_type=SignalType.ABNORMAL_VOLUME_REVERSAL, lag_bars=0)

    with pytest.raises(ValidationError):
        SignalDefinition(
            signal_type=SignalType.ABNORMAL_VOLUME_REVERSAL,
            arbitrary_code="import os",  # type: ignore[call-arg]
        )
