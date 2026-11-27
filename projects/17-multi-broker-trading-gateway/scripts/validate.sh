#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

mkdir -p bin reports/evidence

echo "==> [1/6] Static analysis (go vet)"
go vet ./...

echo "==> [2/6] Config secret hygiene audit (ensuring zero secrets in config/)"
if grep -EIn '(SUPER_SECRET|OAUTH_BEARER_SECRET|HMAC_SIGNING_SECRET|BEGIN PRIVATE KEY|sk_live_)' config/*.json; then
  echo "ERROR: Potential secret literal detected in config/" >&2
  exit 1
fi
echo "OK: No secret literals found in config/*.json"

echo "==> [3/6] Unit, Contract, Integration, and Failure Scenario tests (-race)"
go test -race -count=1 -v ./... | tee reports/evidence/test_suite_output.txt

echo "==> [4/6] Baseline throughput/latency benchmark"
go test -run='^$' -bench=. -benchmem -count=3 ./tests/scenarios/... | tee reports/evidence/benchmark_output.txt

echo "==> [5/6] End-to-end demo run"
go build -o bin/gateway ./cmd/gateway
./bin/gateway -mode demo -config config -out reports/evidence/demo_output.json >/dev/null

echo "==> [6/6] 7-scenario fault & reconciliation drill"
./bin/gateway -mode drill -config config -out reports/evidence/drill_output.json >/dev/null

echo "==> Validation PASSED. Raw evidence written to reports/evidence/"
