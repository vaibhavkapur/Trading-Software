# Market data platform. Everything it ingests is SIMULATED.
# `make help` lists the targets.

SHELL := /bin/sh
GO ?= go
BIN := bin/mdp
COMPOSE := docker compose
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2
HERMETIC := ./tests/unit/... ./tests/scenarios/...
# Loads .env (ClickHouse password, optional ports) into a recipe's shell
# without echoing it.
ENV := set -a; . ./.env; set +a

.DEFAULT_GOAL := help
.PHONY: help setup build lint test integration-test demo bench bench-real replay fixtures \
        deps-up deps-down stack-up stack-down failure-demo logs clean

help: ## list targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

setup: ## download Go modules; create .env with a random local ClickHouse password
	$(GO) mod download
	@if [ ! -f .env ]; then \
	    umask 077; printf 'MDP_CLICKHOUSE_PASSWORD=%s\n' "$$(openssl rand -hex 24)" > .env; \
	    echo "created .env (ignored by git) with a random local password"; \
	else echo ".env exists; left unchanged"; fi

build: ## build bin/mdp
	$(GO) build -o $(BIN) ./src/cmd/mdp

lint: ## gofmt, go vet, staticcheck
	@unformatted="$$(gofmt -l src tests)"; if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	$(GO) vet ./...
	$(GO) run $(STATICCHECK) ./...

test: ## unit and scenario tests with the race detector; no Docker needed
	$(GO) test -race -count=1 $(HERMETIC)

integration-test: deps-up ## the adapters and the whole pipeline against Redpanda and ClickHouse
	@$(ENV); MDP_INTEGRATION=1 \
	    MDP_KAFKA_BROKERS=127.0.0.1:$${MDP_KAFKA_PORT:-19092} MDP_CLICKHOUSE_URL=http://127.0.0.1:$${MDP_CLICKHOUSE_PORT:-8123} \
	    $(GO) test -race -count=1 -timeout 10m -v ./tests/integration/...

demo: ## scripted walkthrough with simulated data, failures included; no Docker needed
	$(GO) run ./src/cmd/mdp demo

bench: ## in-process load test; prints what it measured
	$(GO) run ./src/cmd/mdp bench

bench-real: deps-up ## the same load test through Redpanda and ClickHouse (throwaway namespace and database)
	@$(ENV); $(GO) run ./src/cmd/mdp bench --infra real \
	    --brokers 127.0.0.1:$${MDP_KAFKA_PORT:-19092} --clickhouse-url http://127.0.0.1:$${MDP_CLICKHOUSE_PORT:-8123}

replay: ## replay the committed capture and verify its output digest
	$(GO) run ./src/cmd/mdp replay

fixtures: ## regenerate the committed capture and manifest (deterministic)
	$(GO) run ./src/cmd/mdp fixture

deps-up: ## start Redpanda and ClickHouse and wait until they are healthy
	@[ -f .env ] || { echo 'no .env: run "make setup" first'; exit 1; }
	$(COMPOSE) up -d --wait redpanda clickhouse

deps-down: ## stop Redpanda and ClickHouse (data volumes are kept)
	$(COMPOSE) stop redpanda clickhouse

stack-up: ## build and start everything: simulator, services, gateway on :8080, Prometheus on :9091
	@[ -f .env ] || { echo 'no .env: run "make setup" first'; exit 1; }
	$(COMPOSE) --profile app up -d --build --wait

stack-down: ## stop everything (data volumes are kept)
	$(COMPOSE) --profile app down

failure-demo: ## inject failures into the running stack and audit the store afterwards (needs stack-up)
	./scripts/failure-demo.sh

logs: ## follow the services' logs
	$(COMPOSE) --profile app logs -f --tail=50 ingest normalize sink gateway

clean: ## stop everything and delete this project's containers, volumes and build output
	$(COMPOSE) --profile app down -v --remove-orphans
	rm -rf bin data
