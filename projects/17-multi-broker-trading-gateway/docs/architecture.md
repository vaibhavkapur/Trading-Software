# Multi-Broker Trading Gateway — Architecture

## Overview

The Multi-Broker Trading Gateway (MBTG) provides a unified, fail-closed execution boundary between upstream OMS/strategy systems and heterogeneous broker venues. It isolates external broker contracts behind a single `broker.Adapter` interface, enforces versioned capability and instrument validation prior to dispatch, guarantees idempotent order/fill normalization, and runs an independent reconciliation engine that halts execution on unresolved state drift.

## Component Boundaries

| Component | Package | Responsibility |
|---|---|---|
| Domain Contract & Mapping | `src/domain` | Canonical `Order`, `Fill`, `OrderRequest`, `WebhookEvent`, structured `GatewayError`, versioned `CapabilityMatrix`, bidirectional `InstrumentMapper`, monotonic status ranks & VWAP recomputation. |
| Security & Policy Gate | `src/security` | `SecretManager` interface + `MemorySecretManager`, `SecretString` (always prints/marshals `[REDACTED]`), `RedactingLogger` (scrubs secrets & patterns), deterministic `ExecutionPolicy` (paper/sandbox default; live explicitly denied). |
| Rate Limiting & Clocks | `src/ratelimit` | Per-broker `TokenBucket` enforcing local burst/rate caps and honoring upstream HTTP 429 `Retry-After` windows with injectable `Clock`. |
| Broker Adapter Contract | `src/broker` | Unified `Adapter` interface & concurrency-safe `Registry`. |
| Broker Adapters | `src/adapters/{mock,sandbox_alpha,sandbox_beta}` | Deterministic mock + two distinct sandbox adapters (sync REST/OAuth vs async queue/HMAC/cursor-replay). |
| Persistence & Recovery | `src/storage` | `Store` interface, `MemoryStore`, durable append-only JSONL `JournalStore` with startup replay, plus PostgreSQL DDL (`docs/schema.sql`). |
| Gateway Core | `src/gateway` | Orchestrates fail-closed gate, policy, symbol mapping, capability checks, idempotency, adapter dispatch, webhook deduplication, out-of-order fill handling, cursor replay, and operational metrics. |
| Independent Reconciler | `src/reconciler` | Audits broker ground truth against local store, resolves `SUBMIT_UNKNOWN` and `CANCEL_PENDING` without blind retries, detects phantom/missing orders & quantity drift, and trips fail-closed halt. |
| REST & WebSocket API | `src/api` | Pure Go stdlib HTTP REST endpoints and RFC 6455 WebSocket live event stream (`/v1/ws`). |

## Architecture Diagram

```mermaid
flowchart TD
    Client["OMS / Strategy / Operator CLI"] --> API["REST + WebSocket API (src/api)"]
    API --> GW["Gateway Core (src/gateway)"]

    subgraph PreDispatch["Fail-Safe Pre-Dispatch Gate (Zero Broker Calls on Reject)"]
        Halt["Fail-Closed Circuit Gate"]
        Policy["Execution Policy (Paper/Sandbox Default; Live Denied)"]
        Idem["Idempotency Key + Payload Hash Check"]
        Map["Instrument Mapper (Canonical -> Broker Symbol)"]
        Caps["Capability Matrix (Order Type / TIF / Fractional / Notional)"]
        RL["Per-Broker TokenBucket + Retry-After"]
    end

    GW --> PreDispatch
    PreDispatch --> Store["Durable Store (JournalStore / PostgreSQL DDL)"]
    PreDispatch --> Reg["Broker Registry (src/broker)"]

    Reg --> Mock["Mock Adapter (Paper / Seeded Faults)"]
    Reg --> Alpha["Sandbox Alpha (Sync REST / Bearer OAuth / Webhooks)"]
    Reg --> Beta["Sandbox Beta (Async Queue / HMAC / Cursor Replay)"]

    Secrets["SecretManager (No Secrets in Config/Logs)"] --> Mock
    Secrets --> Alpha
    Secrets --> Beta
    Secrets --> Logger["Redacting JSON Logger"]

    Recon["Independent Reconciler (src/reconciler)"] -->|Read Local Truth| Store
    Recon -->|Query Broker Truth| Reg
    Recon -->|Trip Fail-Closed on Unresolved Drift| Halt
```

## Order Lifecycle & Unknown-Outcome Resolution

```mermaid
stateDiagram-v2
    [*] --> PENDING_SUBMIT: Write-ahead persist
    PENDING_SUBMIT --> PENDING_NEW: Async broker ack (sandbox-beta)
    PENDING_SUBMIT --> ACCEPTED: Sync broker ack
    PENDING_SUBMIT --> FILLED: Immediate marketable fill
    PENDING_SUBMIT --> REJECTED: Rate limit / Auth / Broker reject
    PENDING_SUBMIT --> SUBMIT_UNKNOWN: Transport timeout (NO blind retry)

    SUBMIT_UNKNOWN --> ACCEPTED: Reconciled (found open)
    SUBMIT_UNKNOWN --> FILLED: Reconciled (found filled)
    SUBMIT_UNKNOWN --> REJECTED: Reconciled (absent at broker)

    PENDING_NEW --> ACCEPTED: Async queue settled / replay
    PENDING_NEW --> PARTIALLY_FILLED: Out-of-order fill before ACK
    ACCEPTED --> PARTIALLY_FILLED: Partial fill
    PARTIALLY_FILLED --> FILLED: Final fill (VWAP recomputed)
    ACCEPTED --> CANCEL_PENDING: Cancel timeout
    PARTIALLY_FILLED --> CANCEL_PENDING: Cancel timeout
    CANCEL_PENDING --> CANCELLED: Reconciled terminal cancel
    CANCEL_PENDING --> FILLED: Reconciled filled before cancel
    ACCEPTED --> CANCELLED: Cancel ack
```
