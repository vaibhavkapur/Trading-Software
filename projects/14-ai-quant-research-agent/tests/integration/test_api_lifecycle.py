"""Integration tests for the full FastAPI research lifecycle."""

from __future__ import annotations

from fastapi.testclient import TestClient


def test_end_to_end_research_api_lifecycle(client: TestClient):
    # 0. Health & UI & Datasets
    health = client.get("/health")
    assert health.status_code == 200
    assert health.json()["paper_simulated_only"] is True

    ui = client.get("/")
    assert ui.status_code == 200
    assert "PAPER / SIMULATED RESEARCH" in ui.text

    datasets = client.get("/v1/research/datasets")
    assert datasets.status_code == 200
    assert any(d["id"] == "sim_us_liquid_equities_v1" for d in datasets.json())

    # 1. Create hypothesis & proposed experiment
    resp = client.post(
        "/v1/research/hypotheses",
        json={
            "question": (
                "Does abnormal volume predict next-day returns in a liquid equity universe?"
            ),
            "walk_forward": True,
        },
    )
    assert resp.status_code == 201
    payload = resp.json()
    hyp_id = payload["hypothesis"]["id"]
    exp_id = payload["proposed_experiment"]["id"]

    # 2. Validate experiment
    val_resp = client.post(f"/v1/research/experiments/{exp_id}/validate")
    assert val_resp.status_code == 200
    val_data = val_resp.json()
    assert val_data["passed"] is True
    assert val_data["dataset_checksum_verified"] is True

    # 3. Run experiment synchronously
    run_resp = client.post(
        f"/v1/research/experiments/{exp_id}/run",
        json={"async_mode": False, "idempotency_key": "run-key-001"},
    )
    assert run_resp.status_code == 200
    run_data = run_resp.json()["run"]
    run_id = run_data["id"]
    assert run_data["paper_simulated_only"] is True
    assert len(run_data["walk_forward_folds"]) == 3

    # 4. Fetch stored artifacts
    art_resp = client.get(f"/v1/research/runs/{run_id}/artifacts")
    assert art_resp.status_code == 200
    art = art_resp.json()
    assert art["run_id"] == run_id
    assert "trades" in art["artifacts"]
    assert "equity_curve" in art["artifacts"]
    assert "manifest" in art["artifacts"]

    # 5. Reproducibility replay verification
    repro_resp = client.post(f"/v1/research/runs/{run_id}/reproduce", json={"tolerance": 1e-9})
    assert repro_resp.status_code == 200
    repro = repro_resp.json()
    assert repro["reproducible"] is True
    assert repro["trades_digest_match"] is True
    assert repro["max_sharpe_abs_diff"] == 0.0

    # 6. Fetch evidence-linked research report
    rpt_resp = client.get(f"/v1/research/reports/{hyp_id}")
    assert rpt_resp.status_code == 200
    rpt = rpt_resp.json()
    assert rpt["hypothesis_id"] == hyp_id
    assert run_id in rpt["cited_run_ids"]
    assert rpt["trial_count"] >= 1
    assert len(rpt["citations"]) > 0
    assert rpt["reproducibility_manifest"]["trades_digest"] == run_data["trades_digest"]

    # 7. Check JSON and Prometheus metrics
    metrics_json = client.get("/v1/metrics").json()
    assert metrics_json["runs_completed"] == 1
    assert metrics_json["experiment_reproducibility_rate"] == 1.0

    prom = client.get("/metrics")
    assert prom.status_code == 200
    assert "quant_agent_reproducibility_rate" in prom.text
    assert "quant_agent_research_cycle_seconds" in prom.text
