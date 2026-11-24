# ADR 0001: Single-writer, in-process state rebuilt from the audit log

Status: accepted

## Context

The hardest requirement is "concurrent orders cannot oversubscribe a shared notional limit". Two orders arriving together must not both pass a check against the same headroom. The check reads positions, every open reservation, the rate window and the kill switches, and must be atomic with the write that reserves capacity.

The suggested stack was PostgreSQL for durable state and Redis for hot counters. The second requirement in tension with the first is "no decision without a recorded policy version and reason trace": the evidence write and the state write must not be able to disagree.

## Options

1. **Redis counters with Lua scripts, PostgreSQL for audit.** Atomic per script, but exposure spans several keys (per-symbol and gross) plus the kill switch, so the script grows into the whole rule engine, written in Lua. State and audit live in two stores with no shared transaction: a crash between them leaves a reservation without evidence or evidence without a reservation.
2. **PostgreSQL only, `SELECT … FOR UPDATE` on the account row.** Correct and durable in one transaction. Every decision costs at least one network round trip and a synchronous commit, and the rules are split between SQL and application code.
3. **One process owns the state in memory; one mutex; the audit log is the only durable store and the state is a projection of it.**

## Decision

Option 3. All mutations go through `append event → apply event` under a single mutex, and startup replays the log.

## Consequences

Good:

- Atomic check-and-reserve is a mutex, not a protocol. The oversubscription test races 300 clients and the answer is exact.
- Evidence and state cannot diverge: state is *defined* as the result of applying the log. There is no dual write.
- Recovery and replay testing use the production code path. A recorded session reproduces a bit-identical log.
- No external services to run; the rules are ordinary Go, testable with a fake clock.

Bad, and accepted for now:

- **No high availability.** One process, one machine. If it is down, nothing is approved (fail closed), which is safe but is an outage. A standby that tails the log is the natural next step and is not built.
- **Throughput is bounded by one core and one log.** Every decision is serialised, and the Ed25519 signature and JSON encoding dominate the cost (see benchmarks). Sharding by account would need a rule for limits that span accounts.
- **The log grows forever and restart replays all of it.** Snapshots and rotation are not built.
- **Durability is a knob.** The default syncs every 100 ms; per-record fsync is available and slow. A database would have made this choice for us.
- Running two instances against the same data directory would corrupt the chain. Nothing prevents it yet (no lock file).

Revisit when a second instance is needed for availability, or when sustained load approaches the single-writer ceiling.
