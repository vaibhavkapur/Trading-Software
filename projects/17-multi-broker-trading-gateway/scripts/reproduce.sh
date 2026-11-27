#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

mkdir -p bin reports/evidence
go build -o bin/gateway ./cmd/gateway

echo "==> Reproducing all 7 failure scenarios + fail-closed reconciliation drill"
./bin/gateway -mode drill -config config -out reports/evidence/drill_output.json
echo "==> Saved drill evidence to reports/evidence/drill_output.json"
