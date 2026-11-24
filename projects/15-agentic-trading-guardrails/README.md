# Agentic Trading Guardrails

> Policy-as-code, delegated permissions, one-use signed approvals, and tamper-evident audit controls for AI-driven paper orders.

**Stack:** Go 1.24+ (zero external runtime dependencies), Ed25519 JWT/JWKS, Versioned Policy-as-Code PDP, Isolated Paper Broker Adapter, Hash-Chained JSONL Audit Store, Embedded Audit & Replay Web UI  
**Status:** Implemented (MVP + Stretch Goals — Paper / Simulated Only)

## Problem and outcome

A reusable, fail-closed enforcement gateway sitting between AI agents and trading APIs. It strictly separates what an agent may read (`market:read`, `portfolio:read`), propose (`order:propose`), approve (`approval:grant`), and execute (`order:submit`), making limits composable (`Org -> Team -> Agent`), identity-bound, fixed-point exact, and cryptographically auditable.

## System architecture

```text
AI agent + delegated EdDSA JWT
               |
  Tool permission / HTTP gateway (Single PEP)
               |
  Policy decision point (Org -> Team -> Agent + Explicit Deny)
               |
  Risk checks / freshness / clock-skew guard
               |
  Approval capability (Ed25519, one-use, SHA-256 terms_hash bound)
               |
  Isolated broker adapter (PAPER_SIMULATED only, sealed permit)
               |
  Immutable hash-chained audit + metrics + Replay UI
```

See [docs/architecture.md](docs/architecture.md), [ADR-0001](docs/decisions/0001-fail-closed-capability-enforcement.md), and [docs/runbook.md](docs/runbook.md).

## Implemented vs. Planned

### Implemented
- **Delegated Identity & JWT/JWKS ([src/pkg/identity/identity.go](src/pkg/identity/identity.go))**: Ed25519 (`EdDSA`) short-lived JWT issuance and verification, key rotation with `/.well-known/jwks.json`, immediate identity/JTI revocation, clock-skew bounds, and strict non-escalation checks against registered scopes and accounts.
- **Versioned Policy-as-Code Engine ([src/pkg/policy/policy.go](src/pkg/policy/policy.go))**: Fail-closed default deny, explicit `DENY` overrides `ALLOW`, hierarchical constraint narrowing (`Org -> Team -> Agent`), instrument allowlists/denylists, fixed-point per-order and daily notional limits, order-type and UTC trading-window rules, emergency kill switch, immutable policy SHA-256 versioning, and shadow policy simulator (`POST /v1/policies/simulate`).
- **One-Use Signed Approval Capabilities ([src/pkg/approval/approval.go](src/pkg/approval/approval.go))**: Canonical SHA-256 `terms_hash` over every immutable order term, Ed25519-signed short-lived grants bound to `(proposal_id, proposal_hash, account_id, principal_id)`, mutex-protected atomic consumption, and durable restart-safe replay protection.
- **Isolated Paper-Only Broker Adapter ([src/pkg/broker/paper.go](src/pkg/broker/paper.go))**: Unexported credentials loaded from environment, `BROKER_MODE=LIVE` fails closed, sealed `ExecutionPermit` required on `SubmitOrder` (direct writes blocked at code level), fixed-point `PriceTicks`/`MoneyCents` accounting (zero `float64`), idempotency deduplication, and fault-injection toggles for stale quotes, disconnects, and broker rejections.
- **Immutable Hash-Chained Audit & Replay ([src/pkg/audit/audit.go](src/pkg/audit/audit.go))**: Append-only SHA-256 hash chain (`prev_hash -> entry_hash`) persisted to JSONL with startup verification, automatic secret/token redaction, distinct `event_time_utc` vs `received_at_utc`, correlation IDs, and proposal replay timeline builder.
- **Single PEP Gateway, MCP-Style Tool Proxy & Audit UI ([src/pkg/gateway/](src/pkg/gateway/))**: Full REST API, `POST /v1/tools/call` with untrusted-data tagging and prompt-injection detection, Prometheus/JSON metrics ([src/pkg/metrics/metrics.go](src/pkg/metrics/metrics.go)), durable state store ([src/pkg/store/store.go](src/pkg/store/store.go)), and embedded Audit & Replay Console at `/`.
- **Verification Suites ([tests/](tests/))**:
  - Unit tests: [tests/unit/unit_test.go](tests/unit/unit_test.go)
  - Integration & restart/concurrency tests: [tests/integration/integration_test.go](tests/integration/integration_test.go)
  - Security, resiliency & deterministic replay scenarios: [tests/scenarios/scenarios_test.go](tests/scenarios/scenarios_test.go)
  - Performance profile & benchmark: [tests/perf/perf_test.go](tests/perf/perf_test.go)

### Planned (Future Extensions)
- PostgreSQL and Redis external persistence adapters for multi-replica horizontal scaling.
- gRPC transport bindings alongside the HTTP/JSON gateway API.
- OPA/Rego or Cedar external PDP sidecar plugin interface.

## API endpoints

| Method & Path | Purpose |
| :--- | :--- |
| `GET /` | Embedded Audit & Replay Web UI (`PAPER / SIMULATED ONLY`) |
| `GET /healthz` | Liveness, active policy version, and audit hash-chain status |
| `GET /.well-known/jwks.json` | Ed25519 public JSON Web Key Set |
| `POST /v1/identities/tokens` | Mint short-lived delegated JWT for a registered identity |
| `POST /v1/authorize` | Standalone fail-closed PDP evaluation (supports `notional_cents` or fixed-decimal `notional_usd`) |
| `POST /v1/tools/call` | MCP-style per-tool entitlement proxy (`market.quote`, `portfolio.get`) with prompt-injection containment |
| `POST /v1/proposals` / `GET /v1/proposals/{id}` | Create or inspect hashed order proposals (`order:propose`) |
| `POST /v1/approvals` | Issue signed one-use approval capability bound to `proposal_id` and `proposal_hash` (`approval:grant`) |
| `POST /v1/execute` | Fail-closed policy re-check + atomic grant consume + isolated paper broker fill (`order:submit`) |
| `GET /v1/policies` / `POST /v1/policies` | List or publish versioned policy bundles (`policy:admin`) |
| `POST /v1/policies/simulate` | Shadow-evaluate a candidate policy bundle against sample requests |
| `GET /v1/audit?proposal_id=...` | Query redacted immutable audit log |
| `GET /v1/audit/replay?proposal_id=...` | End-to-end replay view (actor, principal, policy versions, approval, broker response) |
| `GET /v1/audit/verify` | Cryptographically verify SHA-256 hash chain |
| `GET /v1/metrics` | Guardrail telemetry JSON (`?format=prometheus` for Prometheus exposition) |

## Repository layout

```text
.
├── README.md
├── LICENSE
├── Makefile
├── Dockerfile
├── docker-compose.yml
├── go.mod
├── .github/workflows/ci.yml
├── docs/
│   ├── architecture.md
│   ├── decisions/
│   │   └── 0001-fail-closed-capability-enforcement.md
│   └── runbook.md
├── src/
│   ├── cmd/
│   │   ├── gateway/main.go
│   │   └── demo/main.go
│   └── pkg/
│       ├── types/
│       ├── identity/
│       ├── policy/
│       ├── approval/
│       ├── broker/
│       ├── audit/
│       ├── metrics/
│       ├── store/
│       └── gateway/
├── tests/
│   ├── unit/
│   ├── integration/
│   ├── scenarios/
│   └── perf/
├── examples/
│   ├── policies/default-policy.json
│   └── fixtures/replay-scenario.json
└── scripts/
    └── demo.sh
```

## Quick start (verified commands)

```bash
make setup             # Build ./bin/gateway and ./bin/demo
make lint              # Run gofmt check and go vet ./...
make test              # Run unit and security/resiliency scenario tests (-race)
make integration-test  # Run HTTP lifecycle, restart persistence, and concurrency race tests (-race)
make bench             # Run end-to-end authorization latency & memory benchmarks
make demo              # Run simulated walkthrough + fail-closed failure-mode demo
make failure-demo      # Run failure-mode demo only (stale quote, disconnect, tampering, kill switch)
make run               # Start HTTP gateway & Audit UI on http://localhost:8080
```

Or via Docker:

```bash
docker compose up --build
```

## Verified test & performance results

Measured locally on `darwin/arm64` (Apple M5, 10 CPUs, Go `go1.27.1`):

- **Test suites (`make test && make integration-test`):** All 14 test suites pass under `-race`, covering fixed-point arithmetic boundaries, JWT/JWKS rotation & revocation, hierarchical policy narrowing, prompt-injection containment, proposal tampering invalidation, expired/replayed/wrong-account/wrong-principal grant denials, 20-goroutine concurrent replay races, kill-switch override after approval, stale quote & broker disconnect fail-closed handling, restart persistence, audit tamper detection, and deterministic fixture replay.
- **Authorization profile (`make bench` — 2,000 sequential `Authorize` calls including Ed25519 verification, hierarchical PDP evaluation, and SHA-256 hash-chained audit logging):**
  - Throughput: `22,776 ops/sec` (`88 ms` wall time, `0` errors)
  - Latency: `p50 = 37.5 µs`, `p95 = 87.9 µs`, `p99 = 110.4 µs`
  - `BenchmarkAuthorizeEndToEnd-10`: `34,515 ns/op`, `10,943 B/op`, `122 allocs/op`

## Safety, data and operational boundaries

- **Paper/simulated by default.** Live trading is explicitly blocked (`BROKER_MODE=LIVE` returns an error at startup).
- **No floating-point execution math.** Prices use `int64` `PriceTicks` (`$0.0001`) and money uses `int64` `MoneyCents` (`$0.01`) with overflow-checked `math/big` arithmetic.
- **Secret isolation & redaction.** Broker credentials are kept in unexported fields inside `broker.PaperAdapter` and sensitive keys/tokens are redacted before writing audit logs.

## License

MIT — see [LICENSE](LICENSE).
