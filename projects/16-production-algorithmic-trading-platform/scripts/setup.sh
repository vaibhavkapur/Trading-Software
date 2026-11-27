#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

python3 -m uv venv --python 3.12 .venv
python3 -m uv pip install --python .venv/bin/python --offline -e ".[dev]" 2>/dev/null \
  || python3 -m uv pip install --python .venv/bin/python -e ".[dev]"
echo "Setup complete. Activate with: source .venv/bin/activate"
