# M1 — Foundation and Design Report

## Scope Completed
- Audited external foundation references and documented the clean-room IP/licensing decision in [`docs/dependency_and_ip_audit.md`](../docs/dependency_and_ip_audit.md) and [`docs/adr/ADR-001-clean-room-go-architecture.md`](../docs/adr/ADR-001-clean-room-go-architecture.md).
- Defined canonical domain contracts (`Order`, `OrderRequest`, `Fill`, `WebhookEvent`, monotonic `OrderStatus` rank, deterministic VWAP recomputation, and structured `GatewayError` taxonomy) in `src/domain/`.
- Authored versioned capability matrix (`config/capabilities.json`), bidirectional instrument mapping (`config/instruments.json`), execution policy (`config/policy.json`), and PostgreSQL schema (`docs/schema.sql`).
- Implemented secret-safe `SecretManager` + `SecretString` + `RedactingLogger` (`src/security/`) and token-bucket rate limiting with `Retry-After` cooldown support (`src/ratelimit/`).

## Verification Evidence
- Unit test suite (`tests/unit/`): `TestOrderRequestValidationInvariants`, `TestRecomputeOrderFromFillsVWAPAndMonotonicity`, `TestSecretStringAndLoggerNeverLeakSecrets`, `TestExecutionPolicyPaperSandboxDefaultAndLiveDenied`, `TestTokenBucketRateLimitAndRetryAfter`, `TestInstrumentMappingAndCapabilityMatrix` — **ALL PASS** with `-race`.
- Raw output retained in [`reports/evidence/test_suite_output.txt`](evidence/test_suite_output.txt).
