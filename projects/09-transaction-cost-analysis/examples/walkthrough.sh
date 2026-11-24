#!/usr/bin/env bash
# Load the hand-checkable example into a running server and read the results back.
# Start the server first, empty:  PYTHONPATH=src .venv/bin/python -m tca serve
# Simulated data only.
set -euo pipefail

BASE="${TCA_URL:-http://127.0.0.1:8009}"
DIR="$(cd "$(dirname "$0")/worked" && pwd)"

for dataset in orders fills quotes bars; do
  echo "== POST /v1/imports/${dataset}"
  curl -sS -X POST "${BASE}/v1/imports/${dataset}?source=${dataset}.csv" \
    -H 'content-type: text/csv' --data-binary "@${DIR}/${dataset}.csv"
  echo
done

echo "== GET /v1/tca/orders/W-BUY?benchmark=arrival_mid   (expect 60.0000 / 5.9988 bps)"
curl -sS "${BASE}/v1/tca/orders/W-BUY?benchmark=arrival_mid" | python3 -c '
import json, sys
body = json.load(sys.stdin)
print(json.dumps({k: body[k] for k in ("headline", "flags", "methodology_version", "dataset_version")}, indent=2))'

echo "== GET /v1/tca/summary?group_by=venue"
curl -sS "${BASE}/v1/tca/summary?group_by=venue" | python3 -m json.tool

echo "== GET /v1/tca/export.csv"
curl -sS "${BASE}/v1/tca/export.csv"
