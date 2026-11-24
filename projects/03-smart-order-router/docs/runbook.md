# Runbook

All activity is **simulated / paper**. Requires Go 1.27+ (pinned in `go.mod`). No third-party dependencies.

## Commands

```bash
make setup             # verify toolchain, build
make lint              # gofmt + go vet
make test              # unit tests, race detector on
make integration-test  # HTTP API, seeded scenarios, replay, fault fuzzing
make demo              # calm TWAP + replay of every routing decision
make demo-failure      # venue outage + degraded venue, with a failure timeline
make compare           # TWAP vs LIQUIDITY across seeds, vs single-venue baseline
make bench             # micro-benchmarks
make serve             # HTTP API on 127.0.0.1:8080, simulated time at 20x
./scripts/api-demo.sh  # scripted curl walkthrough on a manual clock
```

## CLI

```bash
bin/sor run [-log events.jsonl] [-json] examples/venue-outage.json
bin/sor replay events.jsonl          # exit 1 on any mismatch
bin/sor compare -seeds 1,2,3 examples/*.json
bin/sor serve [-addr 127.0.0.1:8080] [-speed 20] [-config scenario.json] [-log events.jsonl]
```

`-speed` is simulated seconds per wall second. `-speed 0` gives a manual clock driven by `POST /sim/advance`,
which makes API sessions fully reproducible. The server binds to localhost by default and has no authentication;
do not expose it.

## HTTP API

| Method and path | Purpose |
|---|---|
| `POST /parents` | `{"symbol","side","qty","strategy":"TWAP"\|"LIQUIDITY","duration_seconds","interval_seconds"?}` → 201, or 422 with a risk `code` |
| `GET /parents`, `GET /parents/{id}` | Parent state |
| `GET /parents/{id}/children` | Child orders |
| `GET /parents/{id}/fills` | Fills |
| `GET /parents/{id}/execution-quality` | TCA report including the single-venue baseline |
| `POST /parents/{id}/cancel` | Cancel a working parent |
| `GET /venues` | Normalized quotes and venue status |
| `POST /sim/venues/{venue}/faults` | Replace the venue's fault state (see below); `{}` heals |
| `POST /sim/advance` | `{"ms":n}` — manual clock only |
| `POST /kill-switch` | `{"engaged":true\|false}` |
| `GET /metrics` | Prometheus text counters |
| `GET /healthz` | Liveness, simulated time, kill-switch state |

Fault fields: `latency_ms` (added one-way), `reject_rate` (0–1), `down`, `stop_quotes`, `clock_skew_ms`,
`duplicate_rate` (0–1). Each POST replaces the whole fault state for that venue.

## Scenario files

JSON; every field is optional except `parents`. See `examples/`. Top-level keys: `name`, `description`, `seed`,
`symbol`, `lot_size`, `venues[]`, `policy`, `risk`, `router`, `market_view`, `baseline_venue`, `kill_at_ms`,
`parents[]` (`at_ms` + the `POST /parents` body), `faults[]` (`at_ms`, `venue`, `fault`). Defaults live in
`src/engine/config.go`.

## Operating situations

**A venue is misbehaving.** `GET /venues` shows its status. `TRIPPED` clears by itself after
`breaker_cooldown_ms` (default 15s) and one successful trial order. `STALE`/`SKEWED` clear when good quotes
arrive. No action is needed; the router is already avoiding it.

**A parent shows `unresolved_qty` > 0.** Some child timed out and its outcome is unknown, usually because the
venue link dropped mid-flight. The quantity stays reserved so the parent cannot overfill. It resolves when the
link returns (reconciliation) or the late report arrives. See ADR 0001.

**A parent finished `EXPIRED`.** The deadline passed with quantity unfilled. Check `GET /parents/{id}/children`
for rejects and timeouts and the event log for `route_skipped` / `child_blocked` events.

**The kill switch engaged by itself.** The log has an `anomaly` event: a venue report would have overfilled an
order. Routing is stopped and working parents are cancelled. Investigate before releasing with
`POST /kill-switch {"engaged":false}`; cancelled parents are not resumed.

**Stop everything now.** `POST /kill-switch {"engaged":true}`. No new parents or children are accepted. Children
already at a venue still report and are accounted for.

**Reproduce a run.** Re-run the same scenario file: the event log is byte-identical. To audit decisions only,
`bin/sor replay <log>`.

## Event log kinds

`parent_accepted`, `parent_rejected`, `route_decision` (full planner input + decision), `route_skipped`,
`child_blocked`, `child_sent`, `exec_report`, `child_timeout`, `reconcile`, `fault`, `kill_switch`, `anomaly`,
`parent_done`.
