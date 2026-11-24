# Task runner for the matching engine. Everything here is simulated/paper only.
CARGO ?= cargo
FIXTURES := tests/scenarios/fixtures
BENCH_ARGS ?=

.PHONY: setup lint test integration-test test-all demo failure-demo bench criterion fixtures replay-check serve clean

setup: ## Install the pinned toolchain components and fetch dependencies
	rustup show active-toolchain
	$(CARGO) fetch --locked

lint: ## Formatting and clippy, warnings are errors
	$(CARGO) fmt --all -- --check
	$(CARGO) clippy --locked --all-targets -- -D warnings

test: ## Unit, semantic and property tests of the matching core
	$(CARGO) test --locked --lib --test unit

integration-test: ## Journal/recovery, TCP gateway, failure scenarios, replay fixtures
	$(CARGO) test --locked --test integration --test scenarios

test-all: test integration-test

demo: ## Walk through a simulated session and prove replay is byte-identical
	./scripts/demo.sh

failure-demo: ## Kill the gateway mid-session, tear the log, recover
	./scripts/failure-demo.sh

bench: ## Full benchmark report (writes docs/benchmarks/latest.md)
	$(CARGO) run --locked --release --bin me-bench -- --out docs/benchmarks/latest.md $(BENCH_ARGS)

criterion: ## Criterion micro-benchmark of the matching core
	$(CARGO) bench --locked --bench matching

replay-check: ## Replay the pinned fixtures through the CLI and diff the bytes
	@for f in session gen-seed7; do \
		$(CARGO) run --locked -q -- replay $(FIXTURES)/$$f.commands.jsonl 2>/dev/null \
			| cmp - $(FIXTURES)/$$f.events.jsonl && echo "replay-check $$f: byte-identical"; \
	done

fixtures: ## Regenerate pinned fixtures (review the diff before keeping it!)
	$(CARGO) run --locked -q -- run examples/session.jsonl \
		--emit-log $(FIXTURES)/session.commands.jsonl > $(FIXTURES)/session.events.jsonl
	$(CARGO) run --locked -q -- gen --seed 7 --count 500 --symbols 2 \
		| $(CARGO) run --locked -q -- run - \
		--emit-log $(FIXTURES)/gen-seed7.commands.jsonl > $(FIXTURES)/gen-seed7.events.jsonl

serve: ## Run the gateway on 127.0.0.1:9001 with data in ./data
	$(CARGO) run --locked --release -- serve --addr 127.0.0.1:9001 --data-dir data

clean:
	$(CARGO) clean
