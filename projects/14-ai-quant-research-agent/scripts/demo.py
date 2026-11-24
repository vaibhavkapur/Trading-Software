"""End-to-end terminal walkthrough (PAPER / SIMULATED) including failure demos.

Demonstrates:
1. Formulating a structured hypothesis & leakage-safe experiment plan
2. Pre-execution validation, deterministic backtesting, and walk-forward folds
3. Reproducibility replay verification (bit-for-bit trades digest match)
4. Multi-trial execution with multiple-testing disclosure & evidence-linked report
5. Human peer-review sign-off and research memory inspection
6. Failure demos:
   a) Adversarial lookahead / overlap / code-injection rejection
   b) Stale market-data feed rejection
   c) Registry storage disconnect (HTTP 503 fail-closed behavior)
"""

from __future__ import annotations

import argparse
import tempfile
from datetime import UTC, date, datetime
from pathlib import Path

from fastapi.testclient import TestClient

from quant_agent.api.app import ResearchApplicationState, create_app
from quant_agent.config import Settings
from quant_agent.data.catalog import DEFAULT_EQUITY_UNIVERSE
from quant_agent.data.simulator import FaultInjectionConfig


def run_demo(*, quiet: bool = False) -> None:
    def log(msg: str) -> None:
        if not quiet:
            print(msg)

    with tempfile.TemporaryDirectory(prefix="quant_agent_demo_") as tmp:
        settings = Settings(
            data_dir=Path(tmp) / "data",
            mode="PAPER_SIMULATED",
            code_revision="demo-0.1.0",
        )
        app = create_app(settings=settings)
        with TestClient(app) as client:
            log("=" * 78)
            log("AI QUANT RESEARCH AGENT — PAPER / SIMULATED RESEARCH WALKTHROUGH")
            log("=" * 78)

            # 1. Plan hypothesis
            q = "Does abnormal volume predict next-day returns in a liquid equity universe?"
            log(f"\n[1] Research Question: {q}")
            hyp_resp = client.post(
                "/v1/research/hypotheses",
                json={"question": q, "walk_forward": True},
            )
            assert hyp_resp.status_code == 201
            hyp = hyp_resp.json()["hypothesis"]
            exp1 = hyp_resp.json()["proposed_experiment"]
            log(f"    Hypothesis ID : {hyp['id']}")
            sig_type = hyp["signal_def"]["signal_type"]
            lag_bars = hyp["signal_def"]["lag_bars"]
            log(f"    Signal DSL    : {sig_type} (lag={lag_bars} bar)")
            log(f"    Null (H0)     : {hyp['null_hypothesis']}")
            log(f"    Experiment ID : {exp1['id']} (config_hash={exp1['config_hash'][:12]}...)")

            # 2. Validate & Run Trial 1
            val = client.post(f"/v1/research/experiments/{exp1['id']}/validate").json()
            assert val["passed"] is True
            log(
                f"\n[2] Validation    : PASSED (validator={val['validator_version']}, "
                f"checksum_verified={val['dataset_checksum_verified']})"
            )

            run1 = client.post(
                f"/v1/research/experiments/{exp1['id']}/run",
                json={"async_mode": False, "idempotency_key": "demo-trial-1"},
            ).json()["run"]
            tm = run1["test_metrics"]
            ci_lo = tm["bootstrap_sharpe_ci_low"]
            ci_hi = tm["bootstrap_sharpe_ci_high"]
            log(
                f"    Run 1 ({run1['id']}): "
                f"Train Sharpe={run1['train_metrics']['sharpe_ratio']:.2f}, "
                f"Val Sharpe={run1['val_metrics']['sharpe_ratio']:.2f}, "
                f"Test Sharpe={tm['sharpe_ratio']:.2f} "
                f"(95% Boot CI [{ci_lo:.2f}, {ci_hi:.2f}])"
            )
            log(
                f"    Holdout Window: {tm['start_date']}..{tm['end_date']} | "
                f"Trades={tm['num_trades']} | TxCosts=${tm['total_transaction_costs']} | "
                f"Net PnL=${tm['net_pnl']}"
            )
            log(f"    Walk-Forward  : {len(run1['walk_forward_folds'])} folds completed")

            # 3. Verify bit-for-bit reproducibility
            repro = client.post(f"/v1/research/runs/{run1['id']}/reproduce").json()
            assert repro["reproducible"] is True
            log(
                f"\n[3] Replay Check  : reproducible={repro['reproducible']} | "
                f"trades_digest_match={repro['trades_digest_match']} | "
                f"max_sharpe_diff={repro['max_sharpe_abs_diff']}"
            )

            # 4. Run Trial 2 (threshold sensitivity) -> triggers multiple-testing warnings
            exp2_body = {
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
                "seed": 20260102,
            }
            exp2 = client.post("/v1/research/experiments", json=exp2_body).json()
            run2 = client.post(
                f"/v1/research/experiments/{exp2['id']}/run",
                json={"async_mode": False},
            ).json()["run"]
            t2_sharpe = run2["test_metrics"]["sharpe_ratio"]
            t2_count = run2["multiple_testing"]["trial_count"]
            log(f"\n[4] Trial 2 ({run2['id']}): Test Sharpe={t2_sharpe:.2f} (trials={t2_count})")

            # 5. Evidence-linked report + Human Peer Review
            rpt = client.post(f"/v1/research/reports/{hyp['id']}/generate").json()
            log(f"\n[5] Evidence-Linked Report ({rpt['id']}):")
            log(f"    Decision      : {rpt['decision']}")
            n_cit = len(rpt["citations"])
            log(f"    Cited Runs    : {rpt['cited_run_ids']} ({n_cit} verified metric citations)")
            log(f"    MT Warning    : {rpt['multiple_testing_warnings'][0]}")
            log(f"    Failure Cases : {len(rpt['failure_cases'])} diagnosed")

            review = client.post(
                f"/v1/research/reports/{hyp['id']}/review",
                headers={"X-Role": "reviewer"},
                json={
                    "reviewer_id": "lead-quant-reviewer",
                    "decision": "changes_requested",
                    "comments": "Inspect turnover and regime sensitivity across sector ETFs.",
                },
            ).json()
            log(f"    Peer Review   : {review['decision']} by {review['reviewer_id']}")

            # 6. FAILURE DEMO A: Leaky experiment (overlap + negative shift + code injection)
            log("\n" + "-" * 78)
            log("[6a] FAILURE DEMO — Adversarial Leaky Experiment Rejection")
            leaky = {
                "hypothesis_id": hyp["id"],
                "dataset_version_id": exp1["dataset_version_id"],
                "universe": exp1["universe"],
                "signal_def": {
                    **exp1["signal_def"],
                    "feature_shift": -1,
                    "custom_expression": "close.shift(-1)",
                },
                "horizon_days": 5,
                "train_range": {"start": "2021-01-04", "end": "2023-02-01"},
                "val_range": {"start": "2023-01-16", "end": "2023-12-29"},
                "test_range": {"start": "2024-01-15", "end": "2024-12-31"},
            }
            leaky_exp = client.post("/v1/research/experiments", json=leaky).json()
            leaky_run = client.post(f"/v1/research/experiments/{leaky_exp['id']}/run")
            assert leaky_run.status_code == 422
            log(f"     HTTP {leaky_run.status_code} REJECTED -> {leaky_run.json()['detail']}")

            # 6b. FAILURE DEMO B: Stale market data feed
            log("\n[6b] FAILURE DEMO — Stale Market Data Feed Rejection")
            state: ResearchApplicationState = app.state.research
            state.catalog.register_simulated_dataset(
                dataset_id="sim_stale_demo_v1",
                name="Stale Feed Demo",
                source="fault_injector",
                symbols=DEFAULT_EQUITY_UNIVERSE[:4],
                coverage_start=date(2021, 1, 4),
                coverage_end=date(2024, 12, 31),
                available_asof=datetime(2025, 1, 2, tzinfo=UTC),
                faults=FaultInjectionConfig(stale_after_date=date(2024, 6, 1)),
            )
            stale_exp = client.post(
                "/v1/research/experiments",
                json={
                    "hypothesis_id": hyp["id"],
                    "dataset_version_id": "sim_stale_demo_v1",
                    "universe": DEFAULT_EQUITY_UNIVERSE[:4],
                    "signal_def": exp1["signal_def"],
                    "horizon_days": 1,
                    "train_range": exp1["train_range"],
                    "val_range": exp1["val_range"],
                    "test_range": exp1["test_range"],
                },
            ).json()
            stale_val = client.post(f"/v1/research/experiments/{stale_exp['id']}/validate").json()
            assert stale_val["passed"] is False
            err_codes = [i["code"] for i in stale_val["issues"] if i["severity"] == "error"]
            log(f"     Validation PASSED={stale_val['passed']} -> {err_codes}")

            # 6c. FAILURE DEMO C: Storage disconnect
            log("\n[6c] FAILURE DEMO — Registry Storage Disconnect (Fail-Closed 503)")
            state.registry.set_fault_mode(disconnected=True)
            disc = client.get("/v1/research/memory")
            assert disc.status_code == 503
            log(f"     HTTP {disc.status_code} -> {disc.json()['detail']}")
            state.registry.set_fault_mode(disconnected=False)

            # 7. Final Telemetry Summary
            metrics = client.get("/v1/metrics").json()
            log("\n" + "-" * 78)
            log("[7] Service Telemetry Snapshot:")
            log(f"    Runs completed           : {metrics['runs_completed']}")
            log(f"    Reproducibility rate     : {metrics['experiment_reproducibility_rate']:.0%}")
            log(
                f"    Rejected leaky/invalid   : {metrics['rejected_invalid_or_leaky_experiments']}"
            )
            log(f"    Cycle time (p50/p99 sec) : {metrics['research_cycle_time_seconds']}")
            deg = metrics["out_of_sample_degradation_vs_in_sample"]
            log(f"    IS vs OOS degradation    : {deg}")
            log("=" * 78)


def main() -> None:
    parser = argparse.ArgumentParser(description="Run AI Quant Research Agent paper demo.")
    parser.add_argument("--quiet", action="store_true", help="Suppress stdout narrative.")
    args = parser.parse_args()
    run_demo(quiet=args.quiet)


if __name__ == "__main__":
    main()
