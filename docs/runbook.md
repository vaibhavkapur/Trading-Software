# Operator runbook

## Normal operation

Run the setup, validation and demo commands in the [README](../README.md). Keep the complete state directory on a local filesystem. The CLI sets a private umask and takes a nonblocking exclusive lock. One process may own a directory at a time. Each run emits JSON Lines on standard output. Errors go to standard error. Use fresh state for independent experiments and the same directory to resume an existing experiment.

`status` starts the local simulator and performs recovery before printing health. Inspect the SQLite files read-only while the process is stopped for an offline examination. `logs/` contains timestamped QuickFIX wire and event logs for both CompIDs. `sequence/` contains the engine stores. A normal `reconciled` event has zero pending requests and matching sender/receiver counters across peers. `logout` records the final sequence numbers and a closed session.

## Failures and recovery

- **Checksum, message or sequence error.** Inspect protocol reject text, reference tags, and the corresponding sequence numbers in the event and wire logs. QuickFIX requests missing messages, replays application messages, and gap-fills skipped administrative messages. Repeated checksum or resend errors warrant stopping the batch and preserving both journals and the session files.
- **Unknown, stale or already closed order.** Inspect the `order_result` reason. Use the latest acknowledged ClOrdID and original side and quantity for a cancellation. The rejection itself is a durable result.
- **Interrupted execution.** Restart with the same state directory. Reuse the original request IDs to inspect the results. Stable IDs protect against repeated fills when wire replay and outbox recovery overlap. Stop and inspect if reconciliation fails, identity conflicts appear, or an order remains pending after recovery.
- **Disk, lock or integrity error.** Stop writers and preserve the complete state directory. Check free space and ownership, inspect database integrity and the paired FIX logs. Restore the OMS journal, simulator journal and sequence stores together from a stopped-process backup. Manual sequence changes or removal of individual state files can lose evidence. A separate empty directory starts an independent simulation.
- **Measurement.** Run `python3 scripts/benchmark.py` after a release build. It emits hardware, workload, percentiles, variation across independent runs and raw timings. Timing is sensitive to filesystem durability, CPU scheduling and other local load.

## Deployment boundary and outstanding approvals

The binary supports one local batch, one CompID pair, one symbol and USD. Unsupported features include external connectivity, TLS and credential provisioning, live or paper routing, multi-user authorization, partial fills, corrections/busts, market orders, other time-in-force policies, multi-venue routing, price discovery, and high availability. Local trials exercise process death and reconnection. Power-loss durability, torn QuickFIX files and disaster recovery need separate qualification.

A proposed counterparty connection requires an agreed dictionary and message matrix, trading calendar, heartbeat and reset policy, order and execution ID scope, duplicate/replay rules, drop-copy or order-status reconciliation, security transport and credentials, permitted instruments and risk limits. The venue must supply a certification harness and sign off on normal flows, negative cases, recovery and reconciliation. The operator also needs independent risk/policy review, broker and compliance approval, approved data entitlements, confirmed IP terms, monitoring ownership and a rehearsed stop procedure. Paper or real-capital enablement has no switch in this build.
