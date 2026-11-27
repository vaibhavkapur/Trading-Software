# FIX protocol gateway

A local C++20 order-entry gateway and synthetic counterparty exchange FIX 4.4 over loopback TCP. Both sessions persist their sequence numbers and messages. The OMS adapter journals commands before transmission and applies execution reports by stable identity. A successful batch ends with matching OMS and venue journals, matching sequence counters, and a bilateral logout.

## Build and run

Use macOS or Linux with a C++20 compiler, CMake 3.20+, SQLite 3 development headers, Python 3.8+, curl, patch, tar, and a writable local filesystem. Loopback TCP must be available. The first setup downloads a SHA-256-pinned QuickFIX archive. Subsequent builds reuse it from `build/deps/`.

```sh
./scripts/setup
./scripts/validate
./scripts/demo
```

The demo creates a private temporary state directory and prints its location. Its five commands produce six reports, one seven-share fill at USD 100.00, and an explicit rejection for an unknown original order. To continue a simulated session, pass the same state directory on each invocation.

```sh
build/fix-gateway state new N1 SYNTH buy 10 9900
build/fix-gateway state replace R1 N1 SYNTH buy 12 9950
build/fix-gateway state cancel C1 R1 SYNTH buy 12
build/fix-gateway state status
```

Inputs use whole shares and integer USD cents. The binary emits versioned JSON Lines. Each order result includes a stable event ID and event and availability timestamps. Resubmitting an identical command returns its stored results. Reusing its ID with changed terms exits with an error.

## Implementation and evidence

- [Architecture, FIX dialect, OMS interface and recovery](docs/design.md)
- [Operator runbook, scope and certification requirements](docs/runbook.md)
- [Conformance results, demo and benchmark](reports/README.md)
- [Implementation plan and acceptance checklist](plan.md)

This product includes software developed by quickfixengine.org (http://www.quickfixengine.org/). Dependency notices and the local patch rationale are in the [design notes](docs/design.md).
