# Everything here is simulated / paper trading. Requires Go >= 1.27 (see go.mod).
GO      ?= go
LOG     ?= demo-events.jsonl

.PHONY: setup lint test integration-test bench demo demo-failure compare serve build clean

setup: ## verify toolchain and module (no third-party dependencies)
	$(GO) version
	$(GO) mod verify
	$(GO) build ./...

build:
	$(GO) build -o bin/sor ./src/cmd/sor

lint: ## gofmt + go vet
	@out="$$(gofmt -l src tests)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...

test: ## unit tests (race detector on)
	$(GO) test -race -count=1 ./src/...

integration-test: ## HTTP API + seeded scenario / replay / fault-fuzz tests
	$(GO) test -race -count=1 ./tests/...

bench: ## reproducible micro-benchmarks (see docs/test-report.md)
	$(GO) test -run '^$$' -bench . -benchmem ./src/planner ./tests/scenarios

demo: build ## run a calm TWAP, then replay every routing decision from the event log
	./bin/sor run -log $(LOG) examples/calm-twap.json
	./bin/sor replay $(LOG)

demo-failure: build ## venue outage + degraded venue, router vs single-venue baseline
	./bin/sor run -log $(LOG) examples/venue-outage.json
	./bin/sor replay $(LOG)
	./scripts/failure-timeline.sh $(LOG)

compare: build ## strategies x seeds x scenarios vs the single-venue baseline
	./bin/sor compare -seeds 1,2,3,4,5 examples/calm-twap.json examples/venue-outage.json

serve: build ## HTTP API on 127.0.0.1:8080, simulated time at 20x
	./bin/sor serve -speed 20

clean:
	rm -rf bin $(LOG)
