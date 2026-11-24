# Benchmarks

Everything on this page was measured with `src/cmd/bench` on one machine on 2026-11-23. Each row is a **single run**; nothing was averaged and no run was discarded. Treat the figures as an indication of order of magnitude on this hardware, not as a guarantee.

## Method

- **Machine:** Apple M5, 10 logical CPUs, 16 GB RAM, macOS (Darwin 25.5), Go 1.27.1, internal SSD.
- **What is timed:** one `Engine.Evaluate` call, in process, including waiting for the engine lock, building the snapshot, hashing, Ed25519 signing and the audit `write()`. **Not** included: HTTP, JSON request parsing, network. Latency over HTTP has not been measured.
- **Workload:** 50 accounts, 10 symbols, seeded PRNG (`-seed 1`). Limit orders of 1–200 shares within ±0.20 of a 100.00 mid; about 4% are priced through the 3% collar and about 3% are oversized, so rejects are part of the mix. After an ALLOW, outside the timed section: 50% commit and fully fill, 40% release, 10% are abandoned and expire after 30 s. A feeder requotes every symbol every 50 ms.
- **Load shape:** *closed loop* = each worker sends its next order as soon as the previous one returns (measures capacity, hides queueing). *Open loop* = orders are scheduled at a fixed rate with evenly staggered workers and latency is measured from the scheduled send time, so a slow call is charged for the ones it delays.
- **Durability setting:** audit records reach the OS on every call; fsync is per record only where stated.
- **Errors:** none are possible in this harness by construction; every call returns a decision.

Reproduce: `go run ./src/cmd/bench [-workers N] [-rate R] [-n N] [-audit-sync]`.

## Results

| Load shape | Workers | Evaluations | p50 | p90 | p99 | p99.9 | max | Throughput |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Closed loop | 1 | 200,000 | 45 µs | 50 µs | 98 µs | 212 µs | 22 ms | 18,512 /s |
| Closed loop | 8 | 200,000 | 55 µs | 1.12 ms | 1.20 ms | 3.9 ms | 10.7 ms | 15,805 /s |
| Open loop, 2,000 /s | 2 | 40,000 | 44 µs | 53 µs | 0.58 ms | 7.8 ms | 16.8 ms | 2,000 /s |
| Open loop, 2,000 /s | 8 | 40,000 | 56 µs | 137 µs | 15.0 ms | 50.9 ms | 92.5 ms | 2,000 /s |
| Open loop, 5,000 /s | 2 | 100,000 | 47 µs | 55 µs | 0.51 ms | 5.9 ms | 10.4 ms | 5,000 /s |
| Open loop, 5,000 /s | 8 | 100,000 | 97 µs | 209 µs | 9.4 ms | 43.1 ms | 66.4 ms | 5,000 /s |
| Open loop, 10,000 /s | 8 | 200,000 | 1.17 ms | 241 ms | 446 ms | 531 ms | 587 ms | 9,997 /s (saturated) |
| Closed loop, **fsync per record** | 8 | 2,000 | 30.6 ms | 34.0 ms | 41.7 ms | 52.3 ms | 61.1 ms | 112 /s |

Decision mix in the 8-worker closed-loop run: 71,853 ALLOW, 128,147 REJECT, 0 REVIEW; most rejects are exposure limits once accounts fill up. That run wrote 300,676 audit records (782 MB, about 2.6 KB each) and heap in use went from 0.9 MB to 275 MB over 113 GC cycles.

## Reading the numbers

- The service time of one decision is roughly 45 µs on this machine. The engine is a single writer, so throughput tops out near 1 / service time once the commit, fill and release calls that share the lock are counted. In this mix that is about 10,000 evaluations per second: the 10,000 /s open-loop row is past the knee and its latencies are queueing delay.
- The 8-worker closed-loop p90 of about 1.1 ms is time spent waiting for the lock behind other workers, not work.
- **The open-loop tails are partly a harness artifact.** The generator busy-waits to hit its schedule precisely, and the p99 at the same 2,000 /s rate is 0.58 ms with 2 generator threads but 15 ms with 8. The engine's work is identical in both; the difference is generator threads competing with the engine and the garbage collector for cores. The 2-worker rows are the better estimate of the engine's own tail, and even those include multi-millisecond outliers at p99.9 that were not investigated.
- Per-record fsync costs about 30 ms here because macOS `fsync` is a full flush to the device. That figure is dominated by the storage stack and will differ greatly on other systems. Group commit (one fsync covering several records) would be the way to make per-decision durability affordable; it is not implemented.

## Not measured

Latency through the HTTP API; behaviour over hours rather than seconds; restart time as a function of log size; Linux; memory once orders start ageing out of the 24-hour retention window; accounts with thousands of simultaneously open reservations (exposure is recomputed by scanning an account's open reservations on every evaluation, so that case will be slower).

## A defect the benchmark found

The first run ended with 1.65 GB of heap after 200,000 orders: every decision's full reason trace was being kept in memory indefinitely for duplicate detection. The in-memory order record now holds the verdict without the trace (which lives in the audit log) and finished orders are dropped after 24 hours, bringing the same run to 275 MB. `TestFinishedOrdersAreForgottenAfterRetention` covers the new behaviour.
