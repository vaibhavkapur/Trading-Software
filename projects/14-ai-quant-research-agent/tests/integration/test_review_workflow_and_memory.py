"""Integration tests for separated auth roles, human peer review, and research memory."""

from __future__ import annotations

from pathlib import Path

from fastapi.testclient import TestClient

from quant_agent.api.app import create_app
from quant_agent.config import Settings


def test_separated_researcher_and_reviewer_auth_and_memory(tmp_path: Path):
    settings = Settings(
        data_dir=tmp_path / "auth_data",
        require_auth=True,
        researcher_token="res-secret-123",
        reviewer_token="rev-secret-999",
    )
    app = create_app(settings=settings)
    with TestClient(app) as client:
        # Unauthenticated hypothesis creation rejected
        unauth = client.post(
            "/v1/research/hypotheses",
            json={"question": "Does mean reversion work on liquid equities?"},
        )
        assert unauth.status_code == 401

        res_headers = {"X-API-Token": "res-secret-123"}
        rev_headers = {"X-API-Token": "rev-secret-999"}

        created = client.post(
            "/v1/research/hypotheses",
            json={"question": "Does mean reversion work on liquid equities?"},
            headers=res_headers,
        ).json()
        hyp_id = created["hypothesis"]["id"]
        exp_id = created["proposed_experiment"]["id"]

        run_resp = client.post(
            f"/v1/research/experiments/{exp_id}/run",
            json={"async_mode": False},
            headers=res_headers,
        )
        assert run_resp.status_code == 200

        rpt_resp = client.post(
            f"/v1/research/reports/{hyp_id}/generate",
            headers=res_headers,
        )
        assert rpt_resp.status_code == 200

        # Researcher CANNOT self-approve peer review (403)
        self_review = client.post(
            f"/v1/research/reports/{hyp_id}/review",
            json={
                "reviewer_id": "researcher-alice",
                "decision": "approved_for_paper_tracking",
                "comments": "Self approval should be blocked.",
            },
            headers=res_headers,
        )
        assert self_review.status_code == 403

        # Authorized reviewer signs off
        peer_review = client.post(
            f"/v1/research/reports/{hyp_id}/review",
            json={
                "reviewer_id": "senior-quant-bob",
                "decision": "rejected",
                "comments": "Insufficient out-of-sample stability after costs.",
            },
            headers=rev_headers,
        )
        assert peer_review.status_code == 200
        assert peer_review.json()["reviewer_id"] == "senior-quant-bob"

        # Hypothesis status updated and visible in research memory
        hyp_after = client.get(f"/v1/research/hypotheses/{hyp_id}").json()
        assert hyp_after["status"] == "rejected"

        mem = client.get("/v1/research/memory").json()
        assert mem["count"] == 1
        assert mem["hypotheses"][0]["hypothesis_id"] == hyp_id
        assert mem["hypotheses"][0]["status"] == "rejected"
