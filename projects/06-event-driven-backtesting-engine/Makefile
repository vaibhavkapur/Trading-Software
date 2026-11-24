PY      := .venv/bin/python
PYTHON  ?= python3.12
SOURCES := src tests examples scripts
export PYTHONPATH := src

.PHONY: setup lint format test integration-test demo failure-demo bench report clean

setup:  ## create .venv and install dependencies
	$(PYTHON) -m venv .venv
	$(PY) -m pip install --quiet --upgrade pip
	$(PY) -m pip install --quiet -e ".[dev]"

lint:
	$(PY) -m ruff check $(SOURCES)
	$(PY) -m ruff format --check $(SOURCES)

format:
	$(PY) -m ruff check --fix $(SOURCES)
	$(PY) -m ruff format $(SOURCES)

test:  ## unit tests (pure logic, no files or subprocesses)
	$(PY) -m pytest tests/unit

integration-test:  ## CLI, file formats, replay, walk-forward and failure scenarios
	$(PY) -m pytest tests/integration tests/scenarios

demo:  ## deterministic walkthrough on synthetic data: run, replay, walk-forward
	$(PY) -m backtester.demo

failure-demo:  ## how the engine behaves on stale, skewed, duplicated and unwritable inputs
	$(PY) examples/failure_demo.py

bench:  ## measure throughput on this machine (see docs/runbook.md for the methodology)
	$(PY) scripts/benchmark.py

report:  ## write a test report with pass/fail evidence to reports/
	mkdir -p reports
	$(PY) -m pytest -v --junitxml=reports/junit.xml | tee reports/test-report.txt

clean:
	rm -rf data runs reports .pytest_cache .ruff_cache
