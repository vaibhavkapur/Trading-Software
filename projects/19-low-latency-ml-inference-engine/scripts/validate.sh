#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PYTHONPATH="${ROOT_DIR}/src:${PYTHONPATH:-}"

echo "[validate] Verifying Python vs C++20 native numerical parity..."
python3 -m inference_engine.cli --root "${ROOT_DIR}" verify-parity

echo "[validate] Running unit, integration, and failure-scenario test suites..."
mkdir -p "${ROOT_DIR}/reports"
python3 -m unittest discover -s "${ROOT_DIR}/tests" -t "${ROOT_DIR}" -p "test_*.py" -v 2>&1 | tee "${ROOT_DIR}/reports/test_evidence.txt"

echo "[validate] All validations passed."
