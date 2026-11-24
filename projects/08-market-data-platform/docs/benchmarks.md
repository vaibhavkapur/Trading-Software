# Benchmarks

Every number here was measured by `mdp bench` and copied from the files in
[evidence/](evidence/). Nothing is estimated or extrapolated.

**Read this first.**

- The load is simulated: two generated venues, six instruments.
- One run each, on one laptop (Apple M5, 10 logical CPUs, 16 GiB RAM,
  macOS, Go 1.27.1), with other software running, including Docker Desktop
  and unrelated containers. There are no repeated runs and so no variance
  figures. Expect different numbers on a second run.
- In the "real" run the load generator, all services, Redpanda and ClickHouse
  share that one machine, and the two servers run inside Docker Desktop's VM
  as single-node development containers.
- The offered rate was fixed at 2 × 2,000 events/s. That is what the test
  asked for, not a limit that was found. No test here looks for the maximum
  sustainable rate.

These figures say the pipeline works at this rate on this machine. They are
not a capacity claim.

## Method

`mdp bench` builds the same services the deployment runs, in one process.

**Phase 1, paced feed.** Both simulated venues publish at a fixed rate for a
fixed time while a WebSocket client subscribed to every instrument receives
the live stream. Afterwards the store must hold exactly the number of events
published, or the benchmark fails.

**Phase 2, catch-up.** The normalizer and sink are stopped, a backlog is
ingested into the raw topic, then both are restarted and the time until the
store holds everything is measured.

How each figure is obtained:

| Figure | How |
| --- | --- |
| events/s, bytes/s | `mdp_ingest_events_total`, `mdp_ingest_bytes_total` over the phase's wall time |
| receive → normalized, receive → stored | `mdp_pipeline_latency_seconds` histogram. Reported as the **upper edge of the bucket** containing the quantile, so "≤ 6.4 ms" means between 3.2 and 6.4 ms. Buckets double in width |
| receive → WebSocket client | exact: client wall clock at receipt minus the event's `receive_time`, sorted, quantiles taken directly. Both clocks are the same host's |
| consumer lag | maximum of samples taken every 100 ms; peaks between samples are missed |
| catch-up | wall time from restart until the store row count equals the expected total |

All latencies start at `receive_time` (the ingestor's clock when the frame was
read). Venue-to-ingestor time is not included; with a simulated venue on the
same host it would be meaningless.

## Results

Commands: `make bench` and `make bench-real`, i.e.
`mdp bench --rate 2000 --duration 10s --backlog 50000`, the second with
`--infra real`.

### Phase 1: 2 venues × 2,000 events/s for 10 s

| | In-memory log and store | Redpanda + ClickHouse |
| --- | --- | --- |
| Events published and stored | 39,992 | 39,992 |
| Ingest rate | 3,997 events/s, 0.37 MB/s | 3,998 events/s, 0.37 MB/s |
| Gaps / duplicates / reconnects / rejects | 0 / 0 / 0 / 0 | 0 / 0 / 0 / 0 |
| Receive → normalized, p50 / p99 | ≤ 3.2 ms / ≤ 6.4 ms | ≤ 6.4 ms / ≤ 25.6 ms |
| Receive → stored, p50 / p99 | ≤ 204.8 ms / ≤ 409.6 ms | ≤ 204.8 ms / ≤ 409.6 ms |
| Receive → WebSocket client, p50 / p99 / max | 2.83 / 5.96 / 26.10 ms | 4.10 / 16.99 / 31.52 ms |
| WebSocket events dropped for slow consumer | 0 | 0 |
| Max consumer lag sampled, normalizer / sink | 0 / 840 records | 54 / 1,031 records |

### Phase 2: catch-up on a 50,000-event backlog

| | In-memory log and store | Redpanda + ClickHouse |
| --- | --- | --- |
| Unpaced ingest of the backlog | 0.15 s (340,189 events/s) | 0.19 s (266,803 events/s) |
| Normalize and store the backlog | 0.34 s (148,951 events/s) | 0.69 s (72,208 events/s) |
| Rows in store at the end / expected | 89,992 / 89,992 | 89,992 / 89,992 |

Sources: [evidence/bench-memory.txt](evidence/bench-memory.txt),
[evidence/bench-real.txt](evidence/bench-real.txt).

## What the numbers do and do not say

- **Latency is set mostly by batching configuration, not by processing
  cost.** The ingestor lingers up to 5 ms to fill a batch, and the sink
  flushes every 200 ms. Receive → stored sits in the 100–400 ms buckets
  because of that flush interval, in both runs alike. Changing `FlushEvery`
  would move it; no other setting was measured.
- **The "unpaced ingest" figure measures the simulator and the adapters**
  writing to a local log as fast as they can, for well under a second. It is a
  burst over loopback, not a sustained rate.
- **The catch-up rates come from sub-second runs** of 50,000 events. They
  show a restarted consumer drains a backlog much faster than the paced feed
  fills it; they are too short to state a steady-state throughput.
- **The in-memory column measures this project's code** with the broker and
  database replaced by in-process structures. It is useful for seeing
  regressions in the services, and says nothing about Kafka or ClickHouse.
- **Sink lag of about 800–1,000 records is the expected sawtooth**: at 4,000
  events/s a 200 ms flush interval accumulates about 800 records between
  flushes.
- **The run-to-run spread is unknown.** An earlier single run on the same
  machine, before unrelated code changes, gave a WebSocket p99 of 7.18 ms and
  a catch-up of 0.36 s against the real services, where this one gave 16.99 ms
  and 0.69 s. With other workloads on the machine, a factor of two between
  runs should be assumed.

## Not measured

- Maximum sustainable throughput, or behaviour near it.
- Anything on server hardware, on Linux, or with a multi-node cluster.
- Many instruments, many partitions, or many WebSocket clients (one client,
  six instruments here).
- Query latency of the historical API under load, and `FINAL` cost on large
  tables. `mdp_query_duration_seconds` exists; no load test drives it.
- Memory and CPU use.
- Long runs: nothing here ran longer than about four minutes (the failure
  demo), so leaks or slow degradation would not have shown.

## Reproducing

```sh
make bench                         # in-memory
make bench-real                    # needs Docker and .env; uses a throwaway namespace and database
go run ./src/cmd/mdp bench -h      # --rate, --duration, --backlog, --seed, --json
```
