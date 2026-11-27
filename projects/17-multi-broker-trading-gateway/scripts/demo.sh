#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

mkdir -p bin reports/evidence
go build -o bin/gateway ./cmd/gateway

echo "==> Running end-to-end multi-broker demo (mock + sandbox-alpha + sandbox-beta)"
./bin/gateway -mode demo -config config -out reports/evidence/demo_output.json
echo "==> Saved demo evidence to reports/evidence/demo_output.json"
