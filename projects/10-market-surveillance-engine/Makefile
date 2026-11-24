GO ?= go

.PHONY: setup lint test integration-test scenario-test test-all golden fixtures mutation demo evaluate bench build run clean

setup: ## verify the toolchain and module (no third-party dependencies)
	$(GO) version
	$(GO) mod verify
	$(GO) build ./...

lint: ## formatting and static checks
	@out="$$(gofmt -l src tests)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...

test: ## unit tests: normalizer, windows, rules, journal, case log, simulator
	$(GO) test -race -count=1 ./tests/unit/...

integration-test: ## HTTP API, auth, restart recovery, rule reload, concurrency
	$(GO) test -race -count=1 ./tests/integration/...

scenario-test: ## failure scenarios and deterministic replay
	$(GO) test -race -count=1 ./tests/scenarios/...

test-all: lint test integration-test scenario-test

golden: ## re-record the replay transcript after an intended rule change
	$(GO) test -count=1 ./tests/scenarios/ -run TestRecordedSessionReplaysIdentically -update

fixtures: ## regenerate the recorded session from its seed (then run `make golden`)
	$(GO) run ./src/cmd/simgen -seed 7 -n 21 -out tests/scenarios/testdata/session.jsonl \
		-labels tests/scenarios/testdata/session.labels.json

mutation: ## plant deliberate bugs and check the tests catch each one (needs python3)
	python3 scripts/mutation_check.py

demo: ## simulated end-to-end walkthrough, including failure cases
	$(GO) run ./src/cmd/demo

evaluate: ## labelled synthetic scenarios: caught, missed, wrongly flagged
	$(GO) run ./src/cmd/evaluate

bench: ## in-process load test; prints measured throughput and latency
	$(GO) run ./src/cmd/bench

build:
	$(GO) build -o bin/surveild ./src/cmd/surveild

run: build ## start the server with the simulator on; set the SURV_* tokens first
	./bin/surveild -sim -rules examples/rules.json -refdata examples/refdata.json -data data

clean:
	rm -rf bin data
