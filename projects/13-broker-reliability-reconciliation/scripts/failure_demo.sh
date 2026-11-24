#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
go run ./src/cmd/recon failure-demo -fixture examples/fixtures/paper_day.json -policy examples/policies/default_policy.json
