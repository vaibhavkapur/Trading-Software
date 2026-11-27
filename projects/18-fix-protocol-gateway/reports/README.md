# Verification evidence

The release build ran on macOS 26.5.1, arm64, with Apple clang 21.0.0. Both CTest entry points passed. The conformance executable exercises ten named scenarios and the CLI test launches separate processes against the same durable state. [Raw CTest output](conformance.txt)

- A damaged checksum left the target sequence unchanged and produced no execution. The next valid heartbeat exposed the gap and recovered the staged command.
- A missing Side field produced a session reject naming tag 54. An unsupported OrderStatusRequest produced a business reject naming message H and reason 3.
- Dropping a heartbeat and an order produced a ResendRequest, an administrative gap-fill and a replay with PossDupFlag and OrigSendingTime. The journals agreed after recovery.
- A repeated execution with a new transport sequence left the fill count and filled shares unchanged. A conflicting body under the same execution identity halted processing.
- Reconnect recovered a missing trade after a simulated prior-date store restart. A separate child process exited inside the outbound trade send. On restart, session replay and outbox publication overlapped, and a single seven-share fill remained.
- Additional cases covered command invariants, replacement crossing the quote, stale and unknown order IDs, heartbeat echo, two-way logout, crash windows before FIX handoff, exclusive state ownership and idempotent batch reruns.

The demo produced six reports from five synthetic requests. One seven-share trade and one unknown-order rejection were among those reports. The final counter pairs matched and pending requests were zero. [Actual JSONL output](demo.jsonl)

A fresh local clone downloaded the pinned dependency, built the release binaries, passed both CTest targets and completed the demo. Its working tree remained clean. The [GitHub Actions workflow](../../../.github/workflows/fix-protocol-gateway.yml) runs the build, tests and demo on Ubuntu. [Clean checkout record](clean-checkout.txt)

## Latency baseline

On an Apple M5 with ten logical CPUs, 500 measured sequential one-share buys had a median local submission time of 1.406 ms, p95 of 1.965 ms and p99 of 3.212 ms. Across five fresh-state runs the individual p95 values ranged from 1.626 to 2.088 ms. Ten warm-up orders per run were excluded. Each order crossed the fixed synthetic quote and produced an acknowledgment and a filled execution report. The interval includes loopback FIX transport and both sides' journal operations. These measurements characterize this machine and workload. [Environment and all 500 samples](benchmark.json)

## Reproduction

```sh
./scripts/setup
./scripts/validate
./scripts/demo
python3 scripts/benchmark.py
```

During development the domain suite exposed an empty decimal string accepted as zero. The parser now requires digits before the decimal point. On macOS the first process-crash test failed to reach its injection point after forking the existing runtime. The test now executes a fresh child binary, verifies its exit code at the intended boundary and checks recovered state. The checked-in conformance output is from the corrected implementation.
