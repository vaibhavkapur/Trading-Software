# Multi-Broker Trading Gateway

One Go gateway routes orders to a mock broker and two simulated brokers with different acknowledgement and replay behavior. It normalizes orders and fills, journals each request, and reconciles unknown outcomes before retrying. The default policy permits paper and sandbox execution.

## Getting started

The gateway requires Go 1.24 or later and uses only the standard library.

From the repository root, enter the project directory and run the following commands.

```bash
cd projects/17-multi-broker-trading-gateway
./scripts/setup.sh       # Verify toolchain and build bin/gateway
./scripts/validate.sh    # Run go vet, race-checked unit/contract/scenario tests, benchmarks & secret audit
./scripts/demo.sh        # Run end-to-end multi-broker demo (mock + sandbox-alpha + sandbox-beta)
./scripts/reproduce.sh   # Reproduce all 7 failure scenarios + fail-closed reconciliation drill
```

Or start the local REST + WebSocket server (paper/sandbox only):
```bash
./bin/gateway -mode serve -addr 127.0.0.1:8080
```

## Project layout

```text
17-multi-broker-trading-gateway/
  plan.md               # Implementation plan & acceptance checklist
  README.md             # Project overview & quickstart
  go.mod                # Zero-dependency Go module definition
  Makefile              # Build, test, demo, drill, validate targets
  cmd/gateway/          # CLI entrypoint (demo | drill | serve)
  src/
    domain/             # Canonical orders, fills, errors, capability matrix, instrument mapper
    security/           # SecretManager, SecretString, RedactingLogger, ExecutionPolicy
    ratelimit/          # Per-broker TokenBucket + Retry-After cooldown + injectable Clock
    broker/             # Unified Adapter interface and Registry
    adapters/
      mock/             # Deterministic seeded mock adapter with fault injection
      sandbox_alpha/    # Sandbox #1: Sync REST, OAuth Bearer, US:<TICKER>, webhooks
      sandbox_beta/     # Sandbox #2: Async queue, HMAC auth, <TICKER>.US / BTC-USD, cursor replay
    storage/            # Store interface, MemoryStore, write-ahead JSONL JournalStore
    gateway/            # Core order routing, idempotency, webhook dedup, fail-closed circuit, metrics
    reconciler/         # Independent reconciliation engine (unknown outcomes, cancel timeouts, drift halt)
    api/                # HTTP REST API + RFC 6455 WebSocket live event stream
  config/               # Versioned JSON manifests (gateway, instruments, capabilities, policy; NO secrets)
  data/fixtures/        # Synthetic test orders, webhooks, fault scenarios & provenance manifest
  tests/
    unit/               # Domain invariants, redaction, policy, rate-limit, mapping tests
    integration/        # Shared broker contract suite (all 3 adapters), REST/WS & crash recovery tests
    scenarios/          # All 7 required failure scenarios + fail-closed drill + benchmarks
  docs/                 # Architecture, API/schema, PostgreSQL schema.sql, ADRs, security review, runbook
  scripts/              # setup.sh, demo.sh, reproduce.sh, validate.sh
  reports/              # Milestone M1–M4 reports, acceptance checklist, and raw evidence artifacts
```

## Documentation and evidence

- [Architecture & State Diagrams](docs/architecture.md)
- [REST/WebSocket API & Canonical Schema](docs/api_and_schema.md)
- [PostgreSQL Production Schema](docs/schema.sql)
- [Dependency & IP Audit](docs/dependency_and_ip_audit.md)
- [Security Review & Gated Rollout Checklist](docs/security_review.md)
- [Operator Runbook](docs/runbook.md)
- [Unsupported Features & Non-Goals](docs/unsupported_features.md)
- [Acceptance Checklist & Evidence](reports/acceptance_checklist.md)
