GOCACHE ?= $(CURDIR)/.gocache
GO := GOCACHE=$(GOCACHE) go

.PHONY: setup lint test integration-test scenario-test bench demo failure-demo run clean

setup:
	@mkdir -p $(GOCACHE) bin data
	$(GO) mod tidy
	$(GO) build -o bin/gateway ./src/cmd/gateway
	$(GO) build -o bin/demo ./src/cmd/demo
	@echo "Setup complete: binaries built in ./bin"

lint:
	@unformatted=$$(gofmt -l src tests); \
	if [ -n "$$unformatted" ]; then \
		echo "Unformatted files:"; echo "$$unformatted"; exit 1; \
	fi
	$(GO) vet ./...
	@echo "Lint checks passed."

test:
	$(GO) test -race -v ./tests/unit/... ./tests/scenarios/...

integration-test:
	$(GO) test -race -v ./tests/integration/...

scenario-test:
	$(GO) test -race -v ./tests/scenarios/...

bench:
	$(GO) test -v -bench=. -benchmem ./tests/perf/...

demo:
	$(GO) run ./src/cmd/demo -mode=all

failure-demo:
	$(GO) run ./src/cmd/demo -mode=failure

run:
	$(GO) run ./src/cmd/gateway -addr :8080 -data-dir ./data -policy ./examples/policies/default-policy.json

clean:
	rm -rf bin data $(GOCACHE)
