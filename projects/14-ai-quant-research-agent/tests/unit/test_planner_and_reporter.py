"""Unit tests for ResearchPlanner and evidence-linked ResearchReporter guardrails."""

from __future__ import annotations

import pytest

from quant_agent.api.app import ResearchApplicationState
from quant_agent.models.domain import EvidenceCitation
from quant_agent.models.signals import SignalType
from quant_agent.reporter.reporter import CitationVerificationError


def test_planner_maps_questions_to_allowlisted_signals_without_inventing_metrics(
    app_state: ResearchApplicationState,
):
    hyp, exp = app_state.planner.propose(
        "Does abnormal volume predict next-day returns in a liquid equity universe?"
    )
    assert hyp.signal_def.signal_type == SignalType.ABNORMAL_VOLUME_REVERSAL
    assert hyp.null_hypothesis.startswith("H0:")
    assert exp.signal_def.lag_bars >= 1
    assert exp.train_range.end < exp.val_range.start < exp.test_range.start

    # Ensure validation passes out of the box
    val = app_state.validator.validate(exp)
    assert val.passed is True


def test_reporter_refuses_report_without_stored_run_evidence(
    app_state: ResearchApplicationState,
):
    hyp, _ = app_state.planner.propose("Does cross-sectional momentum work in sector ETFs?")
    app_state.registry.save_hypothesis(hyp)

    with pytest.raises(ValueError, match="zero stored RunResults"):
        app_state.reporter.generate_report(hyp.id)


def test_reporter_citation_verification_blocks_fabricated_metrics(
    app_state: ResearchApplicationState,
):
    hyp, exp = app_state.planner.propose("Does mean reversion z-score predict returns?")
    app_state.registry.save_hypothesis(hyp)
    app_state.registry.save_experiment(exp)
    _, run = app_state.runner.submit(exp.id, async_mode=False)
    assert run is not None

    report = app_state.reporter.generate_report(hyp.id)
    assert app_state.reporter.verify_report_citations(report) is True

    # Tamper with a citation metric value (simulate hallucinated Sharpe=9.99)
    tampered = report.model_copy(
        update={
            "citations": [
                EvidenceCitation(
                    claim="Hallucinated Sharpe",
                    run_id=run.id,
                    metric_name="sharpe_ratio",
                    metric_value="9.99",
                    split_name="test",
                )
            ]
        }
    )
    with pytest.raises(CitationVerificationError, match="Metric mismatch"):
        app_state.reporter.verify_report_citations(tampered)
