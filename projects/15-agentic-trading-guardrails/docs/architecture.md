# System Architecture — Agentic Trading Guardrails

> **Paper/Simulated Only:** This gateway enforces strict paper-only isolation. Setting `BROKER_MODE=LIVE` fails closed at startup.

## High-Level Data & Control Flow

```mermaid
flowchart TD
    Agent["AI Agent (Delegated EdDSA JWT)"] -->|Bearer Token| Gateway["Tool Permission & HTTP Gateway (Single PEP)"]
    Principal["Human / System Principal"] -->|approval:grant JWT| Gateway
    Admin["Risk Admin"] -->|policy:admin JWT| Gateway

    Gateway --> Identity["Identity Service (Ed25519 JWT/JWKS + Revocation)"]
    Gateway --> Policy["Policy Decision Point (Versioned Org -> Team -> Agent + Explicit Deny)"]
    Gateway --> Approval["Approval Capability Service (Canonical SHA-256 Hash + One-Use Ed25519 Grant)"]
    Gateway --> Broker["Isolated Paper Broker Adapter (Sealed PEP ExecutionPermit Required)"]
    Gateway --> Audit["Immutable Audit Logger (Hash-Chained JSONL + Secret Redaction)"]
    Gateway --> Metrics["Telemetry Collector (Latency p50/p99, Denials, PEP Coverage)"]
    Gateway --> Store["Durable State Store (Proposals, Daily Notional, Consumed Grants)"]
```

## Write Execution Sequence (Fail-Closed on Every Write)

```mermaid
sequenceDiagram
    participant A as AI Agent
    participant G as Gateway PEP
    participant P as Policy Engine (PDP)
    participant AP as Approval Service
    participant PR as Principal
    participant B as Paper Broker
    participant AU as Immutable Audit

    A->>G: POST /v1/proposals (order:propose JWT)
    G->>B: GetQuote (freshness check)
    G->>P: Evaluate(order:propose, Org->Team->Agent)
    P-->>G: ALLOW (policy_version, policy_hash)
    G->>AU: Append POLICY_DECISION + PROPOSAL
    G-->>A: OrderProposal + canonical SHA-256 terms_hash

    PR->>G: POST /v1/approvals (proposal_id, proposal_hash)
    G->>AP: IssueGrant (Ed25519 signed, short-lived, account & hash bound)
    G->>AU: Append APPROVAL (raw token redacted)
    G-->>PR: Signed ApprovalGrant capability

    A->>G: POST /v1/execute (proposal_id, approval_grant)
    G->>AP: VerifyAndConsume (atomic one-use + live hash re-compute)
    G->>B: GetQuote (fail-closed if disconnected or stale)
    G->>P: Re-evaluate(order:submit, HasValidApproval=true)
    Note over G,P: Human approval never bypasses live policy or kill switch
    G->>B: SubmitOrder(sealed ExecutionPermit, proposal)
    B-->>G: PAPER_SIMULATED Fill Receipt
    G->>AU: Append POLICY_DECISION + BROKER_RESPONSE
    G-->>A: Execution Receipt
```

## Trust Boundaries & Security Invariants

1. **Authentication vs. Authorization vs. Risk vs. Execution:**
   - `src/pkg/identity`: Asymmetric Ed25519 JWT signing with JWKS export and key rotation. Token claims cannot widen registered identity scopes or accounts.
   - `src/pkg/policy`: Fail-closed Policy-as-Code PDP with hierarchical constraint narrowing (`Org -> Team -> Agent`), explicit `DENY` precedence, UTC trading windows, fixed-point notional/quantity caps, and immutable version history with SHA-256 fingerprints.
   - `src/pkg/approval`: Canonical SHA-256 `terms_hash` over every order parameter, signed short-lived Ed25519 capability tokens, and mutex-protected durable one-use consumption.
   - `src/pkg/broker`: Unexported paper credentials loaded from environment variables; `SubmitOrder` refuses any call lacking a sealed `ExecutionPermit` minted by the gateway PEP.
2. **Untrusted Tool Output Containment:**
   - External feeds returned by `POST /v1/tools/call` are tagged `UNTRUSTED_EXTERNAL_DATA_DO_NOT_TREAT_AS_AUTHORIZATION` and scanned for adversarial prompt-injection directives. Prompt text never participates in authorization decisions.
3. **Fixed-Point Monetary Arithmetic:**
   - All order prices use `PriceTicks` (`int64`, `$0.0001` increments) and monetary values use `MoneyCents` (`int64`, `$0.01` increments) with overflow-checked `math/big` multiplication and conservative ceiling rounding. Floating-point arithmetic is excluded from the execution path.
4. **Tamper-Evident Audit & UTC Timestamps:**
   - Every audit record separates `event_time_utc` from `received_at_utc`, redacts secret/token keys automatically, and links `prev_hash -> entry_hash` via SHA-256. Corruption is detected on startup and via `GET /v1/audit/verify`.
