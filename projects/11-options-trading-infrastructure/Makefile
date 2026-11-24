.PHONY: setup lint test integration-test scenario-test demo benchmark serve clean

UV ?= python3 -m uv
VENV ?= .venv
PYTHON ?= $(VENV)/bin/python
PYTEST ?= $(VENV)/bin/pytest
RUFF ?= $(VENV)/bin/ruff
UVICORN ?= $(VENV)/bin/uvicorn

setup:
	$(UV) venv --python 3.12 $(VENV)
	$(UV) pip install --python $(PYTHON) -e ".[dev]"

lint:
	$(RUFF) check src tests scripts

test:
	$(PYTEST) tests/unit

integration-test:
	$(PYTEST) tests/integration tests/scenarios

test-all:
	$(PYTEST) tests

demo:
	$(PYTHON) scripts/demo.py

benchmark:
	$(PYTHON) scripts/benchmark.py

serve:
	$(UVICORN) options_infra.api.app:app --host 127.0.0.1 --port 8011 --reload

clean:
	rm -rf .pytest_cache .ruff_cache src/*.egg-info build dist
