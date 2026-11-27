# Security Review & Gated Rollout Checklist

## 1. Threat Model & Implemented Controls

| Threat | Control Implemented | Verification Evidence |
|---|---|---|
| Secret exposure in config files | `config/*.json` stores only opaque `secret_ref` identifiers, never raw keys/tokens. | Config audit in `scripts/validate.sh` |
| Secret leakage in logs or error strings | `SecretString` masks `String()`, `GoString()`, `MarshalJSON()` to `[REDACTED]`; `RedactingLogger` scrubs registered secret values, `Bearer ...`, and sensitive field names (`token`, `secret`, `authorization`, `api_key`, `hmac`). | `TestSecretStringAndLoggerNeverLeakSecrets`, contract suite, drill log audit (`secret_leak_detected: false`) |
| Unapproved live-capital dispatch | `config/policy.json` sets `deny_live_execution: true`, `permitted_modes: ["paper", "sandbox"]`; `Gateway` defaults empty mode to `paper`. | `TestExecutionPolicyPaperSandboxDefaultAndLiveDenied`, contract suite |
| Blind retry causing duplicate positions | `ErrCodeUnknownOutcome` sets status `SUBMIT_UNKNOWN` and returns error on any duplicate submit with zero re-dispatch; resolved exclusively via `reconciler.ReconcileBroker`. | `TestScenario_UnknownSubmitResult` |
| Unsupported order reaching venue | `CapabilityMatrix` and `InstrumentMapper` run strictly before `adapter.SubmitOrder`; verified zero adapter dispatches. | `TestSharedBrokerContractSuite`, `TestScenario_SymbolMismatch` |
| Silent state divergence / phantom fills | Independent `Reconciler` compares local store vs broker snapshots and automatically trips fail-closed `Gateway.Halt()` on unresolved drift. | `TestScenario_CancelTimeout`, `TestScenario_AsyncReplayAndFailClosedDrill` |

## 2. Gated Rollout Checklist (Before Any Approved Sandbox or Production Deployment)

- [x] **Gate 1 (Local Prototype — Complete):** Shared contract suite passing across `mock`, `sandbox-alpha`, `sandbox-beta`; all 7 failure scenarios passing with race detector (`go test -race`); zero secret leaks in logs; paper/sandbox default enforced.
- [ ] **Gate 2 (Hosted Paper/Sandbox — Pending SAAT Approvals):**
  - [ ] Confirm asset class, strategy horizon, and broker API sandbox legal agreements.
  - [ ] Provision external Secret Manager (Vault / GCP Secret Manager / AWS KMS) with short-lived credentials and role separation.
  - [ ] Provision PostgreSQL using `docs/schema.sql` with TLS, least-privilege DB roles, and automated backup/restore drill.
  - [ ] Configure TLS mutual authentication (mTLS) and RBAC on operator endpoints (`/v1/operator/halt`, `/v1/operator/resume`, `/v1/reconcile`).
  - [ ] Configure webhook HMAC signature verification per real broker provider.
- [ ] **Gate 3 (Real-Capital Production — Explicitly Out of MVP Scope):**
  - [ ] Separate legal, regulatory, compliance, broker certification, penetration test, and independent risk-committee sign-off required before changing `deny_live_execution`.
