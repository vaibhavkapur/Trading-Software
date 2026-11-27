#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PYTHONPATH="${ROOT_DIR}/src:${PYTHONPATH:-}"

echo "[setup] Building C++20 native kernel and benchmark binary..."
make -C "${ROOT_DIR}/src/native" clean all

echo "[setup] Generating deterministic synthetic LOB fixtures and verified v1/v2 model bundles..."
python3 -m inference_engine.cli --root "${ROOT_DIR}" build-fixtures

echo "[setup] Setup complete."
