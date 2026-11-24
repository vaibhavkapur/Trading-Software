"""Integration tests for async job queue, idempotency, retries, and multi-trial tracking."""

from __future__ import annotations

import time

from fastapi.testclient import TestClient


def test_async_job_execution_and_idempotency(client: TestClient):
    created = client.post(
        "/v1/research/hypotheses",
        json={"question": "Does cross-sectional momentum predict weekly returns?"},
    ).json()
    exp_id = created["proposed_experiment"]["id"]

    # Submit async job with idempotency key
    r1 = client.post(
        f"/v1/research/experiments/{exp_id}/run",
        json={"async_mode": True, "idempotency_key": "idem-async-1"},
    )
    assert r1.status_code == 200
    job_id = r1.json()["job"]["job_id"]

    # Duplicate delivery with same idempotency key returns same job without double-running
    r2 = client.post(
        f"/v1/research/experiments/{exp_id}/run",
        json={"async_mode": True, "idempotency_key": "idem-async-1"},
    )
    assert r2.status_code == 200
    assert r2.json()["job"]["job_id"] == job_id

    # Wait for background worker to finish
    deadline = time.time() + 10.0
    final_job = None
    while time.time() < deadline:
        jr = client.get(f"/v1/research/jobs/{job_id}")
        assert jr.status_code == 200
        final_job = jr.json()
        if final_job["status"] in ("completed", "failed"):
            break
        time.sleep(0.05)

    assert final_job is not None
    assert final_job["status"] == "completed"
    assert final_job["run_id"] is not None


def test_multiple_trials_trigger_multiple_testing_disclosure(client: TestClient):
    created = client.post(
        "/v1/research/hypotheses",
        json={"question": "Does abnormal volume predict next-day returns?"},
    ).json()
    hyp = created["hypothesis"]
    exp1 = created["proposed_experiment"]

    # Run trial 1
    r1 = client.post(f"/v1/research/experiments/{exp1['id']}/run", json={"async_mode": False})
    assert r1.status_code == 200
    run1 = r1.json()["run"]
    assert run1["trial_number"] == 1

    # Create a second variant experiment (trial 2) on the same hypothesis
    exp2_payload = {
        "hypothesis_id": hyp["id"],
        "dataset_version_id": exp1["dataset_version_id"],
        "universe": exp1["universe"],
        "signal_def": {**exp1["signal_def"], "zscore_threshold": 2.0},
        "horizon_days": exp1["horizon_days"],
        "train_range": exp1["train_range"],
        "val_range": exp1["val_range"],
        "test_range": exp1["test_range"],
        "purge_days": exp1["purge_days"],
        "embargo_days": exp1["embargo_days"],
        "seed": 20260202,
    }
    exp2 = client.post("/v1/research/experiments", json=exp2_payload).json()
    r2 = client.post(f"/v1/research/experiments/{exp2['id']}/run", json={"async_mode": False})
    assert r2.status_code == 200
    run2 = r2.json()["run"]
    assert run2["trial_number"] == 2
    assert run2["multiple_testing"]["trial_count"] == 2
    assert run2["multiple_testing"]["warning_triggered"] is True

    # Generate report and verify BOTH runs are cited and multiple-testing warnings are present
    rpt = client.post(f"/v1/research/reports/{hyp['id']}/generate").json()
    assert rpt["trial_count"] == 2
    assert set(rpt["cited_run_ids"]) == {run1["id"], run2["id"]}
    assert any("Multiple-testing caution" in w for w in rpt["multiple_testing_warnings"])
    assert any("selection bias" in w for w in rpt["multiple_testing_warnings"])
