# ADR-003: Single-Process Vertical Slice First with SQLite WAL and PostgreSQL-Compatible Schema

- **Status**: Accepted
- **Milestone**: M1 / M2 — Foundation & Core Implementation

## Context

`plan.md` Section 3 states:
> "Define domain types and schemas before choosing a distributed deployment; start with the simplest process boundaries that satisfy the workload."

## Decision

1. Run the MVP vertical slice in a single deterministic Python 3.12 process backed by **SQLite WAL** (`src/trading_platform/storage/sqlite_store.py`) so a clean checkout requires no external database daemon or message broker.
2. Maintain a **PostgreSQL-compatible relational DDL** (`docs/schema.sql`) mirroring every table, uniqueness constraint (`idempotency_key`, `fill_id`), and index so migration to PostgreSQL in a staged paper/sandbox environment requires only swapping the store implementation behind the persistence boundary.
3. Isolate observability behind `TelemetryRecorder` with an optional `span_exporter` hook for OpenTelemetry OTLP collectors.

## Consequences

- Deterministic, sub-second test and fault-drill execution in CI and local developer environments.
- No premature distributed-systems complexity while preserving clean persistence and transport boundaries.
