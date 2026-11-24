# Real-time limit order book: task runner. All data is simulated.
CARGO ?= cargo
UI := src/ui
FIXTURE := examples/fixtures/session-faults.ndjson
FIXTURE_FAULTS := drop@2000:3,dup@5000,reorder@8000,cross@11000,garbage@14000,skew@17000
UNIT_TESTS := --test unit_book --test unit_sequencer --test unit_ladders --test unit_analytics --test unit_wire
SCENARIO_TESTS := --test scenario_faults --test scenario_replay
INTEGRATION_TESTS := --test integration_api --test integration_clickhouse

.PHONY: help setup lint fmt test integration-test test-all ui demo demo-replay fixtures replay bench clean

help: ## List targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

setup: ## Fetch Rust crates and install UI dependencies
	$(CARGO) fetch --locked
	cd $(UI) && npm ci

lint: ## rustfmt check, clippy (warnings are errors), TypeScript type-check
	$(CARGO) fmt --all -- --check
	$(CARGO) clippy --workspace --all-targets --locked -- -D warnings
	cd $(UI) && npm run lint

fmt: ## Format Rust sources
	$(CARGO) fmt --all

test: ## Unit + scenario/replay suites and UI logic tests (no sockets)
	$(CARGO) test --locked -p lob-tests $(UNIT_TESTS) $(SCENARIO_TESTS)
	cd $(UI) && npm test

integration-test: ## API, WebSocket, replay-serving and sink tests over real sockets
	$(CARGO) test --locked -p lob-tests $(INTEGRATION_TESTS)

test-all: test integration-test ## Everything

ui: ## Build the UI into src/ui/dist
	cd $(UI) && npm run build

demo: ui ## Serve the simulated venue + UI on http://127.0.0.1:8080 with fault injection
	$(CARGO) run --release --locked -p stream-api --bin lob -- serve --admin

demo-replay: ui ## Serve a looping replay of the committed capture
	$(CARGO) run --release --locked -p stream-api --bin lob -- serve --admin --source replay --file $(FIXTURE) --speed 1 --loop

fixtures: ## Regenerate the committed capture (changes its pinned digest)
	$(CARGO) run --release --locked -p stream-api --bin lob -- record --out $(FIXTURE) --events 20000 --seed 42 --symbols XYZ,ABC --faults "$(FIXTURE_FAULTS)"

replay: ## Verify the committed capture: checkpoints and output digest
	$(CARGO) run --release --locked -p stream-api --bin lob -- replay --file $(FIXTURE)

bench: ## Compare ladder implementations (prints figures measured on this machine)
	$(CARGO) run --release --locked -p stream-api --bin lob -- bench

clean:
	$(CARGO) clean
	rm -rf $(UI)/dist $(UI)/node_modules
