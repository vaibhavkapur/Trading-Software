#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

echo "==> Checking Go toolchain"
go version

echo "==> Verifying go.mod (zero external dependencies)"
go vet ./...

echo "==> Building bin/gateway"
mkdir -p bin
go build -o bin/gateway ./cmd/gateway

echo "==> Setup complete: $(ls -lh bin/gateway)"
