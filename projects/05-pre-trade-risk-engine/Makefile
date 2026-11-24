GO ?= go

.PHONY: setup lint test integration-test scenario-test test-all golden demo bench run build clean

setup: ## verify the toolchain and module (no third-party dependencies)
	$(GO) version
	$(GO) mod verify
	$(GO) build ./...

lint: ## formatting and static checks
	@out="$$(gofmt -l src tests)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...

test: ## unit tests
	$(GO) test -race -count=1 ./tests/unit/...

integration-test: ## HTTP API, restart recovery, concurrency, kill-switch propagation
	$(GO) test -race -count=1 ./tests/integration/...

scenario-test: ## failure scenarios and deterministic replay
	$(GO) test -race -count=1 ./tests/scenarios/...

test-all: lint test integration-test scenario-test

golden: ## re-record the replay transcript after an intended rule change
	$(GO) test -count=1 ./tests/scenarios/ -run TestRecordedSessionReplaysIdentically -update

demo: ## simulated end-to-end walkthrough, including failure cases
	$(GO) run ./src/cmd/demo

bench: ## in-process load test; prints measured latency and throughput
	$(GO) run ./src/cmd/bench

build:
	$(GO) build -o bin/riskd ./src/cmd/riskd

run: build ## start the server; set RISK_ADMIN_TOKEN and RISK_FEED_TOKEN first
	./bin/riskd -policy examples/policy.json -data data

clean:
	rm -rf bin data
