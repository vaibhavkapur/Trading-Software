"""Adversarial leakage scenario tests — ensuring leaky experiments can NEVER run."""

from __future__ import annotations

from datetime import UTC, datetime

from fastapi.testclient import TestClient


def _create_base_hypothesis(client: TestClient) -> tuple[str, dict]:
    resp = client.post(
        "/v1/research/hypotheses",
        json={"question": "Does abnormal volume predict next-day returns?"},
    )
    assert resp.status_code == 201
    data = resp.json()
    return data["hypothesis"]["id"], data["proposed_experiment"]


def test_direct_run_bypass_on_leaky_overlap_experiment_is_blocked(client: TestClient):
    hyp_id, base = _create_base_hypothesis(client)
    leaky_payload = {
        "hypothesis_id": hyp_id,
        "dataset_version_id": base["dataset_version_id"],
        "universe": base["universe"],
        "signal_def": base["signal_def"],
        "horizon_days": 5,
        # Subtle 1-day overlap between val_range.end and test_range.start
        "train_range": {"start": "2021-01-04", "end": "2022-12-30"},
        "val_range": {"start": "2023-01-16", "end": "2024-01-15"},
        "test_range": {"start": "2024-01-15", "end": "2024-12-31"},
        "purge_days": 5,
        "embargo_days": 2,
    }
    exp = client.post("/v1/research/experiments", json=leaky_payload).json()

    # Attempt to skip /validate and call /run directly — MUST fail with 422!
    run_resp = client.post(f"/v1/research/experiments/{exp['id']}/run", json={"async_mode": False})
    assert run_resp.status_code == 422
    assert "SPLIT_OVERLAP_LEAKAGE" in run_resp.json()["detail"]

    # Telemetry must record the rejected leaky experiment
    metrics = client.get("/v1/metrics").json()
    assert metrics["rejected_invalid_or_leaky_experiments"]["total"] >= 1
    assert (
        metrics["rejected_invalid_or_leaky_experiments"]["by_reason"].get(
            "SPLIT_OVERLAP_LEAKAGE", 0
        )
        >= 1
    )


def test_adversarial_future_shift_and_code_injection_blocked(client: TestClient):
    hyp_id, base = _create_base_hypothesis(client)
    attack_payload = {
        "hypothesis_id": hyp_id,
        "dataset_version_id": base["dataset_version_id"],
        "universe": base["universe"],
        "signal_def": {
            **base["signal_def"],
            "feature_shift": -2,
            "custom_expression": "__import__('os').system('echo pwned')",
        },
        "horizon_days": 1,
        "train_range": base["train_range"],
        "val_range": base["val_range"],
        "test_range": base["test_range"],
    }
    exp = client.post("/v1/research/experiments", json=attack_payload).json()
    val = client.post(f"/v1/research/experiments/{exp['id']}/validate").json()
    assert val["passed"] is False
    codes = {i["code"] for i in val["issues"]}
    assert "NEGATIVE_FEATURE_SHIFT" in codes
    assert "ARBITRARY_CODE_FORBIDDEN" in codes
    assert "FORBIDDEN_CODE_TOKEN" in codes


def test_adversarial_point_in_time_backdated_asof_blocked(client: TestClient):
    hyp_id, base = _create_base_hypothesis(client)
    backdated = {
        "hypothesis_id": hyp_id,
        "dataset_version_id": base["dataset_version_id"],
        "universe": base["universe"],
        "signal_def": base["signal_def"],
        "horizon_days": 1,
        "train_range": base["train_range"],
        "val_range": base["val_range"],
        "test_range": base["test_range"],
        "asof_timestamp_utc": datetime(2024, 6, 1, tzinfo=UTC).isoformat(),
    }
    exp = client.post("/v1/research/experiments", json=backdated).json()
    val = client.post(f"/v1/research/experiments/{exp['id']}/validate").json()
    assert val["passed"] is False
    assert any(i["code"] == "POINT_IN_TIME_VIOLATION" for i in val["issues"])
