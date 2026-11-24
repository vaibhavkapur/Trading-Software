# Runbook

Simulated / paper only.

## Everyday commands

| Task | Command |
|---|---|
| Toolchain + deps | `make setup` |
| Format + clippy | `make lint` |
| Core tests | `make test` |
| Journal/gateway/scenario tests | `make integration-test` |
| Walkthrough | `make demo` |
| Crash/recovery walkthrough | `make failure-demo` |
| Benchmark report | `make bench` (→ `docs/benchmarks/latest.md`) |
| Fixture replay diff | `make replay-check` |

## Running the gateway

```bash
cargo run --release -- serve --addr 127.0.0.1:9001 --data-dir data
# options: --fsync            fsync the command log per command
#          --snapshot-every N snapshot every N commands
```

Talk to it with any line-oriented TCP client:

```bash
printf '%s\n' '{"type":"NEW","order_id":1,"symbol":"XYZ","side":"BUY","qty":100,"price_ticks":10025}' \
  | nc -w 2 127.0.0.1 9001
```

Each command is answered with its events and a `{"type":"DONE","cmd_seq":N,"events":K}`
line; undecodable input gets `{"type":"ERROR","message":"..."}`.

Stop with Ctrl-C or SIGTERM: open connections are dropped, queued commands
are drained, and a final snapshot is written.

## Data directory

```text
data/
  commands.jsonl   source of truth (sequenced commands, append-only)
  events.jsonl     derived event feed (append-only)
  snapshots/snapshot-<seq>.json
```

Back up `commands.jsonl`. Everything else can be rebuilt from it.

## Inspecting state without starting the gateway

```bash
cargo run -- recover --data-dir data         # summary: last_seq, tops, open orders
cargo run -- replay data/commands.jsonl      # full event stream on stdout
```

`recover` truncates a torn final line in `commands.jsonl` (that is its only
write).

## Failure modes

| Symptom | Meaning | Action |
|---|---|---|
| Client receives `ERROR ... journal write failed, halting` then `engine halted` | The command log could not be written (disk full, I/O error). The failed command was **not** applied. | Fix the disk problem, restart the gateway. State is recovered from the log. |
| Start-up prints `torn_tail=true` | The previous process died mid-write. The partial record was never acknowledged and has been truncated. | None. |
| Start-up fails with `corrupt log ...:LINE` | A line that is not the last one does not parse. | Do not delete it blindly: every later command depends on it. Restore `commands.jsonl` from backup, or truncate the log at that line knowingly accepting the loss of everything after it, and remove newer snapshots. |
| Start-up fails with `gap in command sequence: expected N, got M` | Records are missing from the middle of the log, or a snapshot is older than the start of the log. | Restore the log from backup. |
| `warning: ignoring snapshot ...` | A snapshot is unreadable or fails validation; an older one (or a full replay) is used. | Delete the bad snapshot file. |
| `warning: snapshot failed` at runtime | Snapshot could not be written; the engine continues on the command log. | Check disk space; recovery will simply replay more. |
| Client disconnects mid-command | A complete line is processed (the reply is discarded); a partial line is ignored. Resting orders are **not** canceled on disconnect. | Clients should reconcile via cancel/replace responses; a retried `NEW` with the same id is rejected as `DUPLICATE_ORDER_ID` while the order is live. |

## Changing behaviour safely

1. Change code, run `make lint test integration-test`.
2. If the pinned fixtures fail, the change altered observable behaviour.
   Decide whether that is intended. If so: `make fixtures`, review the diff of
   `tests/scenarios/fixtures/*.events.jsonl`, and record the reason.
3. Old command logs replayed with new rules may produce different events —
   see ADR 0001.

## Benchmarking notes

- Always `--release`; `me-bench` warns on debug builds.
- Close other workloads; results on a laptop move noticeably between runs.
- The report states CPU, OS, compiler, build profile, load profile and timer
  resolution. Do not quote numbers without the report they came from.
