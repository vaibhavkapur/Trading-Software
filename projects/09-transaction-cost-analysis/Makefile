PYTHON ?= python3.12
VENV   := .venv
BIN    := $(VENV)/bin

export PYTHONPATH := src

.PHONY: setup lint format test integration-test demo run fixtures golden report reconcile clean

setup: ## create the virtualenv and install the package with dev dependencies
	$(PYTHON) -m venv $(VENV)
	$(BIN)/pip install --quiet --upgrade pip
	$(BIN)/pip install --quiet -e ".[dev,parquet]"

lint: ## static checks and formatting check
	$(BIN)/ruff check src tests scripts
	$(BIN)/ruff format --check src tests scripts

format:
	$(BIN)/ruff check --fix src tests scripts
	$(BIN)/ruff format src tests scripts

test: ## unit tests: importer, benchmarks, attribution, aggregation, report
	$(BIN)/python -m pytest tests/unit

integration-test: ## store, HTTP API and CLI, plus the failure and replay scenarios
	$(BIN)/python -m pytest tests/integration tests/scenarios

demo: ## terminal walkthrough on the simulated fixture; writes reports/demo
	$(BIN)/python -m tca demo

run: ## API and dashboard on http://127.0.0.1:8009 with the fixture loaded
	$(BIN)/python -m tca serve --load examples/data

fixtures: ## regenerate examples/data from the simulator (deterministic)
	$(BIN)/python -m tca simulate --out examples/data

golden: ## regenerate the committed report in examples/report
	$(BIN)/python -m tca report --data examples/data --out examples/report

report: ## write a report bundle for the fixture to reports/latest
	$(BIN)/python -m tca report --data examples/data --out reports/latest

reconcile: ## recompute the committed report from raw inputs without the tca package
	$(BIN)/python scripts/reconcile.py --data examples/data --report examples/report

clean:
	rm -rf $(VENV) .pytest_cache .ruff_cache reports data src/*.egg-info
	find . -name __pycache__ -type d -prune -exec rm -rf {} +
