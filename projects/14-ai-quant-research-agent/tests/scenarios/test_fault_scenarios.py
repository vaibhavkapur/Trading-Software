"""Operational fault scenarios: missing data, clock skew, stale events, duplicates, storage loss."""

from __future__ import annotations

from datetime import UTC, date, datetime

from fastapi.testclient import TestClient

from quant_agent.api.app import ResearchApplicationState
from quant_agent.data.catalog import DEFAULT_EQUITY_UNIVERSE
from quant_agent.data.simulator import FaultInjectionConfig
from tests.conftest import make_valid_experiment, make_valid_hypothesis


def _register_faulty_dataset(
    state: ResearchApplicationState, dataset_id: str, faults: FaultInjectionConfig
) -> None:
    state.catalog.register_simulated_dataset(
        dataset_id=dataset_id,
        name=f"Faulty Dataset ({dataset_id})",
        source="fault_injector",
        symbols=DEFAULT_EQUITY_UNIVERSE[:4],
        coverage_start=date(2021, 1, 4),
        coverage_end=date(2024, 12, 31),
        available_asof=datetime(2025, 1, 2, tzinfo=UTC),
        faults=faults,
    )


def test_scenario_stale_market_data_rejected(app_state: ResearchApplicationState):
    _register_faulty_dataset(
        app_state,
        "sim_stale_feed_v1",
        FaultInjectionConfig(stale_after_date=date(2024, 6, 1)),
    )
    hyp = make_valid_hypothesis(
        dataset_id="sim_stale_feed_v1", universe=DEFAULT_EQUITY_UNIVERSE[:4]
    )
    exp = make_valid_experiment(hyp, dataset_version_id="sim_stale_feed_v1")

    res = app_state.validator.validate(exp)
    assert res.passed is False
    assert any(i.code == "DATA_STALE_BARS" for i in res.issues)


def test_scenario_clock_skew_negative_and_excessive_rejected(app_state: ResearchApplicationState):
    _register_faulty_dataset(
        app_state,
        "sim_clock_skew_neg_v1",
        FaultInjectionConfig(clock_skew_seconds=-120),  # receive_time before event_time
    )
    hyp = make_valid_hypothesis(
        dataset_id="sim_clock_skew_neg_v1", universe=DEFAULT_EQUITY_UNIVERSE[:4]
    )
    exp = make_valid_experiment(hyp, dataset_version_id="sim_clock_skew_neg_v1")
    res = app_state.validator.validate(exp)
    assert res.passed is False
    assert any(i.code == "CLOCK_SKEW_VIOLATION" for i in res.issues)

    _register_faulty_dataset(
        app_state,
        "sim_clock_skew_high_v1",
        FaultInjectionConfig(clock_skew_seconds=3600),  # 1 hour skew > 300s limit
    )
    exp2 = make_valid_experiment(hyp, dataset_version_id="sim_clock_skew_high_v1")
    res2 = app_state.validator.validate(exp2)
    assert res2.passed is False
    assert any(i.code == "CLOCK_SKEW_VIOLATION" for i in res2.issues)


def test_scenario_missing_data_gaps_and_duplicate_delivery_rejected(
    app_state: ResearchApplicationState,
):
    _register_faulty_dataset(
        app_state,
        "sim_missing_bars_v1",
        FaultInjectionConfig(missing_date_stride=3),  # drops 33% of bars
    )
    hyp = make_valid_hypothesis(
        dataset_id="sim_missing_bars_v1", universe=DEFAULT_EQUITY_UNIVERSE[:4]
    )
    exp = make_valid_experiment(hyp, dataset_version_id="sim_missing_bars_v1")
    res = app_state.validator.validate(exp)
    assert res.passed is False
    assert any(i.code == "MISSING_DATA_GAPS" for i in res.issues)

    _register_faulty_dataset(
        app_state,
        "sim_dup_bars_v1",
        FaultInjectionConfig(duplicate_every_n=50),
    )
    exp_dup = make_valid_experiment(hyp, dataset_version_id="sim_dup_bars_v1")
    res_dup = app_state.validator.validate(exp_dup)
    assert res_dup.passed is False
    assert any(i.code == "DUPLICATE_BARS_DETECTED" for i in res_dup.issues)


def test_scenario_storage_disconnect_and_rejected_writes(client: TestClient):
    state: ResearchApplicationState = client.app.state.research

    # Read-only storage rejects writes with 503 while reads remain available
    state.registry.set_fault_mode(read_only=True)
    write_resp = client.post(
        "/v1/research/hypotheses",
        json={"question": "Does volume predict returns?"},
    )
    assert write_resp.status_code == 503
    assert "read-only" in write_resp.json()["detail"]

    read_resp = client.get("/v1/research/hypotheses")
    assert read_resp.status_code == 200

    # Complete storage disconnect returns 503 cleanly on both reads and writes
    state.registry.set_fault_mode(disconnected=True)
    disc_resp = client.get("/v1/research/memory")
    assert disc_resp.status_code == 503
    assert "disconnected" in disc_resp.json()["detail"]

    # Recover storage and confirm normal operation resumes
    state.registry.set_fault_mode(disconnected=False, read_only=False)
    ok_resp = client.post(
        "/v1/research/hypotheses",
        json={"question": "Does volume predict returns after recovery?"},
    )
    assert ok_resp.status_code == 201
