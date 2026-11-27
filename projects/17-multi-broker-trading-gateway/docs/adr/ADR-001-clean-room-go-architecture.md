# ADR-001: Clean-Room Go Architecture with Zero External Dependencies

- **Status:** Accepted
- **Date:** 2026-11-27
- **Context:** `plan.md` recommends Go or Rust with REST/WebSocket, PostgreSQL schema, secret management, and contract tests, and explicitly mandates implementing against clean documented contracts when access/IP licensing of external personal repositories is unconfirmed.
- **Decision:** Implement the Multi-Broker Trading Gateway in Go (`go 1.24+`, verified on `go1.27.1`) using the Go standard library exclusively, including a clean RFC 6455 WebSocket broadcaster, JSON REST server, write-ahead JSONL `JournalStore` (with PostgreSQL DDL in `docs/schema.sql`), and a race-checked contract/scenario test suite.
- **Consequences:**
  - Any clean checkout builds and passes all unit, integration, contract, and scenario tests 100% offline with zero supply-chain bloat.
  - Strong static typing, built-in race detector (`go test -race`), and deterministic benchmarking (`go test -bench`).
  - External PostgreSQL and cloud Vault/KMS drivers plug in cleanly behind `storage.Store` and `security.SecretManager` interfaces in Gate 2.
