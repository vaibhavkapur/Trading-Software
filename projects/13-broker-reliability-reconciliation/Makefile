.PHONY: setup build lint test unit-test integration-test scenario-test bench demo failure-demo serve clean

GO ?= go
BIN := bin/recon

setup:
	$(GO) version
	$(GO) mod tidy
	mkdir -p bin

build: setup
	$(GO) build -o $(BIN) ./src/cmd/recon

lint:
	@unformatted=$$(gofmt -l ./src ./tests); \
	if [ -n "$$unformatted" ]; then \
		echo "Unformatted Go files:"; echo "$$unformatted"; exit 1; \
	fi
	$(GO) vet ./...

unit-test:
	$(GO) test -race -v ./tests/unit/...

integration-test:
	$(GO) test -race -v ./tests/integration/...

scenario-test:
	$(GO) test -race -v ./tests/scenarios/...

test:
	$(GO) test -race -v ./tests/unit/... ./tests/integration/... ./tests/scenarios/...

bench:
	$(GO) test -v -bench=. -benchmem ./tests/performance/...

demo:
	./scripts/demo.sh

failure-demo:
	./scripts/failure_demo.sh

serve: build
	./$(BIN) serve -addr :8080 -fixture examples/fixtures/paper_day.json -policy examples/policies/default_policy.json

clean:
	rm -rf bin .recon-state coverage.out
