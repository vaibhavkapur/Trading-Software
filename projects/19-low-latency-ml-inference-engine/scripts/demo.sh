#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PYTHONPATH="${ROOT_DIR}/src:${PYTHONPATH:-}"

echo "[demo] Running end-to-end versioned routing, canary, reload, rollback, and fail-safe demo..."
python3 -m inference_engine.cli --root "${ROOT_DIR}" demo
