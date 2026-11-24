PY      := .venv/bin/python
PYTHON  ?= python3.12
export PYTHONPATH := src

.PHONY: setup lint format test integration-test demo run report clean

setup:  ## create .venv and install pinned-range dependencies
	$(PYTHON) -m venv .venv
	$(PY) -m pip install --quiet --upgrade pip
	$(PY) -m pip install --quiet -e ".[dev,llm]"

lint:
	$(PY) -m ruff check src tests
	$(PY) -m ruff format --check src tests

format:
	$(PY) -m ruff check --fix src tests
	$(PY) -m ruff format src tests

test:  ## unit tests (pure logic, no HTTP)
	$(PY) -m pytest tests/unit

integration-test:  ## API, restart/replay and failure-scenario tests
	$(PY) -m pytest tests/integration tests/scenarios

demo:  ## deterministic terminal walkthrough on simulated data
	$(PY) -m atp.demo

run:  ## start the API + web UI on http://127.0.0.1:8000 (simulator trades 24/7)
	ATP_MARKET_SESSION=always $(PY) -m atp

report:  ## write a test report with pass/fail evidence to reports/
	mkdir -p reports
	$(PY) -m pytest -v --junitxml=reports/junit.xml | tee reports/test-report.txt

clean:
	rm -rf data reports .pytest_cache .ruff_cache
