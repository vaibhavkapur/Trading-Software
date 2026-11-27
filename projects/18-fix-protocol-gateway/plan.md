# FIX Protocol Gateway — Implementation Plan

| Field | Value |
|---|---|
| Collection entry | [18 · FIX Protocol Gateway](../../README.md#projects) |
| Classification | New capability |
| Priority | Strategy-dependent |
| Proposed owner | CTO / trading engineering (reviewers to be assigned) |
| Suggested stack | C++20 or Rust, FIX library such as QuickFIX, TCP/TLS, structured persistence |
| Initial deployment | Local reproducible prototype, then approved paper/sandbox environment |
| Status | Local simulator MVP implemented and verified. External deployment and SAAT ownership confirmation remain open. |

## 1. Objective and scope

Implement a session-aware FIX gateway for simulated order entry and execution reports, with explicit recovery and venue-specific dialect handling.

**MVP deliverables**
- FIX session state machine
- Message validation and mapping
- Persistent sequence store and resend handling
- Simulator and conformance scenarios
- OMS adapter and operational telemetry

**Explicit non-goals**
- No claim of exchange certification or direct connectivity
- no custom protocol shortcuts

## 2. Existing foundations and integration dependencies

- Greenfield capability in the prior GitHub coverage audit. Reuse common contracts and fixtures only after checking ownership and compatibility.

Start with a small independently runnable vertical slice. Do not create duplicate backtesters, order managers, ledgers or data ingestion services when a reviewed shared interface already exists.

**Dependencies / related plans:** #13, #14; existing OMS and OUCH gateway as design reference (not FIX implementation).

Confirm access, licenses, background IP and SAAT ownership terms before copying or adapting any personal repository. A linked prototype is not proof of production readiness. If access or licensing is unresolved, implement against clean, documented contracts instead.

## 3. Proposed architecture and component boundaries

| Component / workstream | Responsibility and boundary |
|---|---|
| FIX session state machine | Own the canonical domain contract and validate incoming data; publish versioned inputs. |
| Message validation and mapping | Implement the core deterministic or research processing; isolate optional external dependencies. |
| Persistent sequence store and resend handling | Persist results or state with reproducible provenance; expose explicit failure modes. |
| Simulator and conformance scenarios | Expose integration or operational interfaces; apply authorization and fail-safe validation. |
| OMS adapter and operational telemetry | Deliver reviewable evidence, operator workflow and integration tests; do not bypass approvals. |

### Suggested repository layout

```text
fix-protocol-gateway/
  plan.md
  README.md
  docs/              # architecture, ADRs, assumptions, runbook
  src/               # domain contracts, core logic, adapters
  config/            # versioned settings; no secrets
  data/fixtures/     # permitted or synthetic inputs and manifests
  tests/unit/
  tests/integration/
  tests/scenarios/
  scripts/           # setup, demo, reproduce, validate
  reports/           # evaluation, benchmarks, acceptance evidence
```

Adapt the directory layout to the language and existing repository. Define domain types and schemas before choosing a distributed deployment; start with the simplest process boundaries that satisfy the workload. Use stable identifiers, explicit units/currency where relevant, UTC event and availability timestamps, versioned configuration and structured errors. Separate external provider interfaces from business logic.

## 4. Implementation milestones

Milestones are ordered by dependency, not promised calendar dates. Each milestone ends with a runnable demo and passing relevant tests.

### M1 — Foundation and design

- [x] Select FIX version/dialect and test counterparty; define supported messages and session policy.
- [x] Document interfaces, assumptions and design decisions introduced in this milestone.
- [x] Add automated tests and store a reproducible demonstration or validation report.

### M2 — Core implementation

- [x] Implement logon, heartbeat, test request, sequence handling and persistence using a vetted library.
- [x] Document interfaces, assumptions and design decisions introduced in this milestone.
- [x] Add automated tests and store a reproducible demonstration or validation report.

### M3 — Integration and validation

- [x] Map new/cancel/replace orders and execution reports to canonical OMS; implement resend/gap-fill and reconnect.
- [x] Document interfaces, assumptions and design decisions introduced in this milestone.
- [x] Add automated tests and store a reproducible demonstration or validation report.

### M4 — Hardening and delivery

- [x] Run simulator conformance, fault injection and latency benchmarks; document counterparty certification requirements.
- [x] Document interfaces, assumptions and design decisions introduced in this milestone.
- [x] Add automated tests and store a reproducible demonstration or validation report.

## 5. Testing and validation

Implement unit tests for domain invariants, integration tests for adapter contracts and scenario tests for failure paths. Use fixed seeds where applicable; avoid testing only happy paths.

| Scenario | Required verification |
|---|---|
| Invalid checksum | Reproduce the case with a fixture or injected fault; assert the documented correct outcome, rejection or recovery and retain evidence. |
| Missing required field | Reproduce the case with a fixture or injected fault; assert the documented correct outcome, rejection or recovery and retain evidence. |
| Sequence gap | Reproduce the case with a fixture or injected fault; assert the documented correct outcome, rejection or recovery and retain evidence. |
| Duplicate execution report | Reproduce the case with a fixture or injected fault; assert the documented correct outcome, rejection or recovery and retain evidence. |
| Reconnect | Reproduce the case with a fixture or injected fault; assert the documented correct outcome, rejection or recovery and retain evidence. |
| Resend | Reproduce the case with a fixture or injected fault; assert the documented correct outcome, rejection or recovery and retain evidence. |
| Logout | Reproduce the case with a fixture or injected fault; assert the documented correct outcome, rejection or recovery and retain evidence. |
| Unknown order | Reproduce the case with a fixture or injected fault; assert the documented correct outcome, rejection or recovery and retain evidence. |

For quantitative work, use chronological splits, point-in-time inputs, realistic costs where relevant, and report failed experiments as well as successful ones. For execution systems, test idempotency, uncertain outcomes and state recovery. Never infer live performance from a simulated test.

## 6. Acceptance criteria / definition of done

- [x] Session recovery preserves sequence integrity.
- [x] Duplicate execution reports do not duplicate fills.
- [x] Unsupported messages are rejected with explicit reason.
- [x] OMS reconciliation succeeds after disconnect.
- [x] Benchmark reports hardware and workload.
- [x] A clean checkout can run the documented setup, tests and demo with permitted fixtures.
- [x] Architecture, dependency assumptions, known limitations and runbook are committed.
- [x] Relevant CI checks pass and raw evaluation or test evidence is retained.
- [x] Security, data licensing, IP and any deployment approval requirements are reviewed.


### Local MVP evidence

- M1 and M2 selected the SYNTH/USD FIX 4.4 dialect, pinned the engine and specified the session policy. [Design and contract](docs/design.md)
- M3 and M4 exercised the order lifecycle, checksum and field errors, unsupported messages, gap-fill, replay, duplicate executions, unknown orders, logout and recovery after process death. [Conformance output](reports/conformance.txt) · [Demo output](reports/demo.jsonl)
- A fresh checkout downloaded the pinned engine and passed the documented setup, local CI tests and demo. The [GitHub Actions workflow](../../.github/workflows/fix-protocol-gateway.yml) runs conformance checks on changes to the gateway. [Clean checkout evidence](reports/clean-checkout.txt)
- The benchmark retained hardware, workload, trial variation and 500 raw observations. [Benchmark report](reports/README.md)
- Local controls, dependency licenses, synthetic-data provenance, limitations and outstanding deployment approvals are documented. External certification, ownership confirmation and any paper or real-capital deployment require separate decisions. [Runbook](docs/runbook.md)

## 7. Operational controls and rollout

- Use synthetic or explicitly permitted historical data first; record provenance and entitlements.
- Store credentials outside code, separate roles, and restrict access to sensitive artifacts.
- Keep order execution disabled unless a deterministic, independently reviewed policy explicitly permits paper/sandbox operation. Any real-capital deployment is a separate approval decision requiring applicable legal/regulatory, broker, security and risk checks.
- Capture health, failure and decision metrics; document the operator response and safe fallback.
- Establish baseline measurements before setting throughput, latency, accuracy or availability targets; report environment, sample size and uncertainty rather than claiming unmeasured performance.

## 8. Handoff and evidence

Deliver source code, dependency lockfile or reproducible build instructions, architecture diagram, API/schema documentation, test report, demo script, benchmark/evaluation artifacts where applicable, and an explicit list of unsupported features. The project is complete at MVP scope only after its acceptance checklist has evidence attached.

**Next decision:** confirm the actual SAAT asset class, strategy horizon, data access, operating constraints and code ownership before selecting implementation depth or connecting real providers.
