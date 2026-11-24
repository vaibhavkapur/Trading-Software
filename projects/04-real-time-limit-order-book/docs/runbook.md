# Runbook

Applies to the simulated/replayed service in this repository.

## Run

```bash
make setup          # once
make demo           # http://127.0.0.1:8080  (simulated venue, fault controls enabled)
make demo-replay    # same UI on a looping replay of the committed capture
make replay         # offline verification of the committed capture
```

Useful flags (`lob serve --help`): `--rate`, `--symbols`, `--stale-after-ms`, `--resync-retry-ms`,
`--snapshot-every`, `--speed`, `--loop`, `--admin`.

`--admin` enables unauthenticated control routes. The server binds to `127.0.0.1` by default; do not
combine `--admin` with a public bind address.

## What the signals mean

| Signal | Meaning | Action |
|---|---|---|
| UI badge **Live** / `lob_synced 1`, `lob_feed_stale 0` | Book is synced and receiving | none |
| **Stale feed** / `lob_feed_stale 1` | Server received nothing for `--stale-after-ms` | Check the source. Prices on screen are the last known ones and are dimmed. |
| **Stale** ("nothing received from the stream API") | Browser heard nothing from the server | Check the server process / network path to it |
| **Disconnected** | WebSocket closed; the UI retries every second | Check the server |
| **Resyncing** / `lob_synced 0` | Gap detected, waiting for a snapshot | Normally clears within the snapshot round trip. If not, see below. |
| `crossed` / `lob_book_crossed 1` | Best bid above best ask, as published by the feed | Informational for a feed that can cross; otherwise suspect loss that sequence numbers did not show |
| `drift_detected` / `lob_drift_total` | Book differed from a same-sequence snapshot | Treat as a bug or upstream corruption; capture and replay the session |
| `decode_error` | Frame could not be parsed | Usually followed by a gap; inspect the frame in the capture |
| `clock_skew` | Venue timestamps ahead of local clock | Check NTP on this host; venue-vs-local time differences are unreliable meanwhile |
| `lob_sink_rows_dropped_total` rising | ClickHouse slow, down or rejecting | Book is unaffected. Check ClickHouse and the server log for the rejection text. |

## Stuck in `resyncing`

1. `curl -s localhost:8080/v1/books/XYZ/health` — look at `unsynced_for_ms`, `buffered`, `last_seq`.
2. `status` events on the stream (or the UI timeline) show `resync_requested` repeating every retry
   interval: requests are going out but no usable snapshot is coming back.
3. `stale_snapshot` events mean snapshots are arriving but are older than the book: the snapshot
   source is lagging the delta stream.
4. `snapshot_applied … still have a hole` means the snapshot predates the buffered deltas; the next
   retry normally fixes it.
5. With a replay source, snapshots cannot be requested; the book recovers at the next snapshot in the
   capture.

## Reproducing an incident

```bash
lob record --out incident.ndjson --events 50000 --seed 7 --faults "drop@12000:40,reorder@30000"
lob replay --file incident.ndjson            # exit code 1 if digest or any checkpoint disagrees
lob serve --source replay --file incident.ndjson --speed 0.25 --admin
```

A capture is `{recv_ts, raw}` per line: the venue's bytes plus the local receive time. Its
`.manifest.json` holds instruments, the fault schedule, ground-truth checkpoints and the digest of
everything the engine published. `scripts/failure-demo.sh` walks through the live failure paths.

## ClickHouse and Prometheus (optional)

```bash
CLICKHOUSE_PASSWORD=choose-one docker compose up --build
```

Credentials come from the environment only. Schema is applied from `scripts/clickhouse-schema.sql`.
This stack has **not** been run as part of the verification in `docs/test-report.md`; the sink is
tested against a stand-in HTTP server.
