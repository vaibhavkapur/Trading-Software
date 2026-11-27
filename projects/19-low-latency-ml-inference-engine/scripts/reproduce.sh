#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PYTHONPATH="${ROOT_DIR}/src:${PYTHONPATH:-}"

"${ROOT_DIR}/scripts/setup.sh"
"${ROOT_DIR}/scripts/validate.sh"
"${ROOT_DIR}/scripts/demo.sh"

echo "[reproduce] Running M3 cold/warm/batch/concurrent latency benchmark suite..."
python3 -m inference_engine.cli --root "${ROOT_DIR}" benchmark --version 1.0.0

echo "[reproduce] Full reproducible pipeline completed. Evidence stored under ${ROOT_DIR}/reports/."
