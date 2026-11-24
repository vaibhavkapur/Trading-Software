.PHONY: setup lint test integration-test scenario-test replay demo bench build clean

GO ?= go
BIN_DIR := bin

setup:
	$(GO) version
	$(GO) mod verify
	@mkdir -p $(BIN_DIR)
	@echo "Setup complete (zero external Go dependencies)."

lint:
	@unformatted=$$(gofmt -l src tests); \
	if [ -n "$$unformatted" ]; then \
		echo "Unformatted files:"; echo "$$unformatted"; exit 1; \
	fi
	$(GO) vet ./...

test:
	$(GO) test -race -v ./tests/unit/...

integration-test:
	$(GO) test -race -v ./tests/integration/...

scenario-test:
	$(GO) test -race -v ./tests/scenarios/...

test-all:
	$(GO) test -race -v ./...

replay:
	$(GO) run ./src/cmd/arb-replay \
		-feed examples/fixtures/recorded_books.jsonl \
		-instruments examples/config/instruments.json \
		-fees examples/config/fees.json \
		-balances examples/config/balances.json \
		-alert-config examples/config/alert_config.json \
		-verify-determinism=true

demo:
	$(GO) run ./src/cmd/arb-demo

bench:
	$(GO) test -bench=. -benchmem ./tests/unit/...

build: setup
	$(GO) build -o $(BIN_DIR)/arb-monitor ./src/cmd/arb-monitor
	$(GO) build -o $(BIN_DIR)/arb-replay ./src/cmd/arb-replay
	$(GO) build -o $(BIN_DIR)/arb-demo ./src/cmd/arb-demo

clean:
	rm -rf $(BIN_DIR) coverage.out data
