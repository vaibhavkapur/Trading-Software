"""Unit tests for ExperimentRegistry immutability, audit events, and research memory."""

from __future__ import annotations

import pytest

from quant_agent.api.app import ResearchApplicationState
from quant_agent.registry.store import ImmutabilityError
from tests.conftest import make_valid_experiment, make_valid_hypothesis


def test_registry_enforces_immutability_on_hypotheses_experiments_and_runs(
    app_state: ResearchApplicationState,
):
    hyp = make_valid_hypothesis()
    exp = make_valid_experiment(hyp)
    app_state.registry.save_hypothesis(hyp)
    app_state.registry.save_experiment(exp)

    with pytest.raises(ImmutabilityError):
        app_state.registry.save_hypothesis(hyp)

    with pytest.raises(ImmutabilityError):
        app_state.registry.save_experiment(exp)

    _, run = app_state.runner.submit(exp.id, async_mode=False)
    assert run is not None

    with pytest.raises(ImmutabilityError):
        app_state.registry.save_run(run)

    audit = app_state.registry.list_audit_events(limit=20)
    actions = {(e["entity_type"], e["action"]) for e in audit}
    assert ("hypothesis", "created") in actions
    assert ("experiment", "created") in actions
    assert ("run", "completed") in actions
