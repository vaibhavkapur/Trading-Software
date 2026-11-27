#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

if [[ ! -d ".venv" ]]; then
  ./scripts/setup.sh
fi

echo "==> 1/5 Ruff lint"
.venv/bin/ruff check src tests scripts

echo "==> 2/5 Pytest unit, integration, and 8-scenario test suite"
mkdir -p reports/evidence
.venv/bin/pytest -v --junitxml=reports/evidence/pytest_junit.xml

echo "==> 3/5 End-to-end paper vertical slice & shadow mode demo"
.venv/bin/python scripts/demo.py > /dev/null

echo "==> 4/5 Mandatory 8-scenario fault drills"
.venv/bin/python scripts/fault_drills.py > /dev/null

echo "==> 5/5 Controlled paper soak & baseline latency measurement"
.venv/bin/python scripts/soak_and_benchmark.py > /dev/null

echo "ALL VALIDATION CHECKS PASSED. Evidence retained in reports/evidence/."
