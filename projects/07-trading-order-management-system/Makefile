GO ?= go

.PHONY: setup lint test integration-test scenario-test test-all golden demo bench build run walkthrough env up down clean

setup: ## verify the toolchain, fetch and verify dependencies, build everything
	$(GO) version
	$(GO) mod download
	$(GO) mod verify
	$(GO) build ./...

lint: ## formatting and static checks
	@out="$$(gofmt -l src tests)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...

test: ## unit tests: pure domain rules, money, adapters. No database.
	$(GO) test -race -count=1 ./tests/unit/...

# The integration and scenario suites run against a real PostgreSQL 17. By
# default they download and start a throwaway one (cached under .cache/); set
# OMS_TEST_DATABASE_URL to use an existing server instead.
integration-test: ## API, idempotency, fills, outbox, cancel/replace, reconciliation
	$(GO) test -race -count=1 ./tests/integration/...

scenario-test: ## failure scenarios, concurrent load with faults, deterministic replay
	$(GO) test -race -count=1 ./tests/scenarios/...

test-all: lint test integration-test scenario-test

golden: ## re-record the replay fixture after an intended behaviour change
	$(GO) test -count=1 ./tests/scenarios/ -run TestReplay_DeterministicGoldenHistory -update

demo: ## simulated end-to-end walkthrough, including the failure demo; exits non-zero if any check fails
	$(GO) run ./src/cmd/demo

bench: ## load test on this machine; prints measured latency, throughput and memory
	$(GO) run ./src/cmd/bench

build: ## build the service and the simulated broker into bin/
	$(GO) build -trimpath -o bin/omsd ./src/cmd/omsd
	$(GO) build -trimpath -o bin/brokersim ./src/cmd/brokersim

run: ## run everything locally without Docker (throwaway database, simulated broker)
	$(GO) run ./src/cmd/devstack

walkthrough: ## curl through the API of a running `make run` or `make up`
	./scripts/api-walkthrough.sh

env: ## generate .env with fresh random secrets for docker compose
	./scripts/gen-env.sh

up: ## start PostgreSQL, the simulated broker and omsd with docker compose (run `make env` first)
	docker compose up --build -d

down:
	docker compose down

clean:
	rm -rf bin .local
