# Runbook

For the local compose stack (`make stack-up`). All data is simulated. Commands
run from the project root.

## Where things are

| What | Where |
| --- | --- |
| Gateway API, `/metrics`, `/healthz` | http://127.0.0.1:8080 |
| Prometheus | http://127.0.0.1:9091 |
| Simulator fault injection | `POST http://127.0.0.1:9103/faults/{alpha\|beta}/{fault}` |
| Kafka API (Redpanda) | 127.0.0.1:19092 |
| ClickHouse HTTP | http://127.0.0.1:8123, user `mdp`, password in `.env` |
| Service logs | `make logs` |

Ports are bound to 127.0.0.1 and can be changed in `.env` (see `.env.example`).

Two helpers used below. Neither puts the password on a command line or in
your shell history: the query runs inside the container, which already has it.

```sh
ch()  { docker compose exec -T clickhouse sh -c 'clickhouse-client --user "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD" --database "$CLICKHOUSE_DB" --query "$0"' "$1"; }
rpk() { docker compose exec -T redpanda rpk "$@"; }
```

## Is it healthy?

```sh
curl -s http://127.0.0.1:8080/v1/health
```

- `store.ok` false: ClickHouse is unreachable from the gateway.
- `ingest[].connected` false: that provider connection is down.
- `instruments[].stale` true: the gateway has seen nothing for that instrument
  for 3 s, on the gateway's own clock.
- `ingest[].lag_ms`: receive time minus venue time. It spans two clocks; a
  large or negative value means a clock is off or the venue is delayed, not
  necessarily that the platform is slow.

```sh
rpk group describe mdp-normalizer   # TOTAL-LAG: raw events not yet normalized
rpk group describe mdp-sink         # TOTAL-LAG: canonical events not yet stored
```

## Signals worth an alert

| Signal | Meaning | First look |
| --- | --- | --- |
| `mdp_consumer_lag_records{consumer="sink"}` growing | store slow or down, or sink stopped | `mdp_sink_retrying`, sink logs, ClickHouse |
| `mdp_consumer_lag_records{consumer="normalizer"}` growing | normalizer stopped or broker slow | normalizer logs |
| `mdp_sink_retrying == 1` for more than a minute | every insert is failing | [ClickHouse down](#clickhouse-is-down-or-rejecting-everything) |
| `increase(mdp_gaps_total[5m]) > 0` | provider data was lost and could not be recovered | [A gap was declared](#a-gap-was-declared) |
| `increase(mdp_sink_rejected_rows_total[5m]) > 0` | the store refused rows; they are in `md.rejects` | [Dead letters](#dead-letters) |
| `increase(mdp_normalizer_rejects_total[5m]) > 0` | a provider sent input that fails validation | [Dead letters](#dead-letters) |
| `mdp_ingest_connected == 0` | provider connection down | ingest logs; the ingestor reconnects with backoff |
| `rate(mdp_ingest_reconnects_total[5m])` high | flapping connection | network, provider |
| `increase(mdp_ws_dropped_total[5m]) > 0` | a WebSocket client is too slow; it has been told | usually the client's problem |
| `increase(mdp_flagged_events_total{flag="clock_skew"}[5m]) > 0` | a venue clock is ahead of the ingestor's | NTP on the ingestor host first |

Latency: `histogram_quantile(0.99, sum by (le, stage) (rate(mdp_pipeline_latency_seconds_bucket[5m])))`.

## Failure handling

### ClickHouse is down or rejecting everything

Expected behaviour: the sink retries the same batch with backoff and reads
nothing new. Nothing is lost; the backlog sits in the canonical topics (7-day
retention). `GET /v1/trades` and the other queries return `503
STORE_UNAVAILABLE`. WebSocket delivery continues.

1. `docker compose ps clickhouse`, `docker compose logs --tail=50 clickhouse`.
2. Authentication failures and a missing table are treated as transient on
   purpose: they are configuration problems that an operator fixes, after
   which the same rows succeed. Check `MDP_CLICKHOUSE_PASSWORD` and that the
   sink logged a successful start-up (`Migrate` creates the tables).
3. Once ClickHouse answers, the sink drains by itself. Watch
   `rpk group describe mdp-sink` return to `TOTAL-LAG 0`.
4. If the outage will outlast 7 days, raise retention on the canonical topics
   first: `rpk topic alter-config md.trades md.quotes md.depth md.gaps --set retention.ms=<ms>`.

### The broker is down

Ingestors block on publish and stop reading from the provider; normalizer,
sink and gateway retry with backoff. On recovery the ingestors reconnect and
ask the provider to resend from their last acknowledged position. Whatever the
provider can no longer resend becomes a declared gap. No action is needed
beyond restoring the broker; then run the [audit](#audit-is-anything-missing-or-doubled).

### A service crashed or was restarted

Safe at any point, including `kill -9`. Each service resumes from its saved
position and may repeat work, which the deduplication keys absorb. After a
normalizer restart the canonical topics can contain repeats; the store and
WebSocket subscribers do not see them.

Run one instance of each service. Two normalizers or two sinks do not corrupt
anything but do the work twice.

### A gap was declared

```sh
curl -s 'http://127.0.0.1:8080/v1/gaps?instrument_id=venueB:XYZ.B'
ch "SELECT provider, session, seq_domain, from_seq, to_seq, recv_ts FROM feed_gaps FINAL ORDER BY recv_ts DESC LIMIT 20 FORMAT PrettyCompact"
```

A gap means the provider skipped `from_seq..to_seq`, the ingestor asked once
for a resend, and the provider did not supply them. The platform will not ask
again. If the missing events can be obtained some other way as a capture file,
`mdp ingest --replay <file>` ingests it. Replayed events are tracked as their
own sequence stream and labelled `source: replay`. The store key does not
include the source, so a replayed event that was missing fills the hole and
one that is already stored collapses onto the existing row (the later write
wins, and the row then reads `source: replay`). That follows from the keys; no
test exercises a mixed live and replay overlap. The gap record stays: it is
the history of what the live feed lost.

A gap starting at the provider's first sequence number right after a start
means the platform joined a session late and the provider could not resend its
beginning.

### Dead letters

```sh
rpk topic consume md.rejects --offset start --num 20 --format '%v\n'
```

Each record has `stage` (`ingest`, `normalize`, `sink`), `reason`, `detail`
and the original payload.

| Reason | Stage | Meaning |
| --- | --- | --- |
| `UNPARSEABLE_FRAME` | ingest | the frame is not valid for the provider's wire format |
| `PRICE_NOT_ON_TICK`, `QUANTITY_NOT_ON_LOT` | normalize | value is not an exact multiple of the instrument's unit; check `tick_size`/`lot_size` in reference data before blaming the provider |
| `UNKNOWN_INSTRUMENT`, `UNKNOWN_PROVIDER` | normalize | missing from reference data; add it and restart |
| `EVENT_TIME_OUT_OF_RANGE`, `MISSING_EVENT_TIME` | normalize | broken venue timestamp |
| `CROSSED_QUOTE`, `CROSSED_BOOK`, `UNSORTED_DEPTH`, … | normalize | internally inconsistent market data |
| `OLDER_THAN_RETENTION` | sink | the event is already older than its table keeps |
| `STORE_REJECTED` | sink | ClickHouse refused the row itself; `detail` has its message |

Rejected provider events still consume their sequence number, so they are not
also reported as gaps.

There is no tool to re-process rejected events. Fixing reference data affects
events normalized after the restart. The rejected events remain in
`md.rejects` (14 days) and, as parsed provider events, in `md.raw` (24 hours).

### A WebSocket client reports `slow_consumer`

The client's queue (1,024 messages) overflowed and it missed `dropped` events
for that instrument. Its live view has a hole; the store does not. The client
should refetch the affected range from `/v1/trades` (or `quotes`, `depth`) and
either read faster or subscribe to fewer instruments or channels.

## Audit: is anything missing or doubled?

For every provider sequence domain, each sequence number between the first and
last stored is either stored once or covered by a declared gap. Run it when
the pipeline has drained (both `TOTAL-LAG` values 0), otherwise in-flight
events show up as absent.

```sql
WITH present AS (
  SELECT provider, session, seq_domain, min(seq) AS lo, max(seq) AS hi, uniqExact(seq) AS stored
  FROM (SELECT provider, session, seq_domain, seq FROM trades FINAL
        UNION ALL SELECT provider, session, seq_domain, seq FROM quotes FINAL
        UNION ALL SELECT provider, session, seq_domain, seq FROM depth FINAL)
  GROUP BY provider, session, seq_domain),
declared AS (
  SELECT g.provider AS provider, g.session AS session, g.seq_domain AS seq_domain,
         sum(least(g.to_seq, p.hi) - greatest(g.from_seq, p.lo) + 1) AS missing
  FROM feed_gaps AS g FINAL
  INNER JOIN present AS p ON p.provider = g.provider AND p.session = g.session AND p.seq_domain = g.seq_domain
  WHERE g.to_seq >= p.lo AND g.from_seq <= p.hi
  GROUP BY provider, session, seq_domain)
SELECT p.provider, p.session, p.seq_domain, p.lo AS first_seq, p.hi AS last_seq, p.stored,
       (p.hi - p.lo + 1) - p.stored AS absent, d.missing AS declared_missing
FROM present AS p LEFT JOIN declared AS d
  ON d.provider = p.provider AND d.session = p.session AND d.seq_domain = p.seq_domain
ORDER BY p.provider, p.session, p.seq_domain
```

`absent` must equal `declared_missing` on every row. Three things make them
differ legitimately:

- events rejected by the normalizer (they have a sequence number and no row):
  compare with `md.rejects`;
- events older than retention, already expired;
- the pipeline had not drained.

`make failure-demo` runs this audit after each of six injected failures.

Always count with `FINAL`. `SELECT count() FROM trades` without it includes
redelivered rows that have not been merged away yet and will read high.

## Routine operations

| Task | How |
| --- | --- |
| Start / stop | `make stack-up` / `make stack-down` (volumes kept) |
| Remove everything of this project | `make clean` (containers, volumes, `bin/`, `data/`) |
| Change retention of a table | `ALTER TABLE trades MODIFY TTL event_ts + INTERVAL <n> DAY`, and change `storage.DefaultRetention` to match so API responses report it |
| Add an instrument | edit the reference data file, restart all services |
| Change the partition count | not supported in place: services refuse to start if a topic's count differs. Create a new namespace (`MDP_KAFKA_NAMESPACE`) and re-ingest |
| Export a day to Parquet | `mdp archive --date 2026-01-05 --out data/archive` |
| Rotate the ClickHouse password | change it in ClickHouse and in the secret source that sets `MDP_CLICKHOUSE_PASSWORD`, restart sink and gateway. Locally: `make clean`, delete `.env`, `make setup` |
| Inject a fault | `curl -X POST 'http://127.0.0.1:9103/faults/beta/lose?n=5'`; faults: `disconnect`, `drop`, `lose`, `duplicate`, `garbage`, `offtick`, `stale`, `skew`, `pause`, `resume`, `newsession` (`n=` count, `ms=` duration, `id=` session) |

## Secrets

- The only secret is `MDP_CLICKHOUSE_PASSWORD`. Supply it by environment
  injection from a secret manager. `.env` is a local-development convenience,
  is ignored by git and should stay mode 600.
- The entitlements file contains token digests, not tokens. Distribute tokens
  out of band.
- Do not paste the password or a token into a command line, an issue, a log or
  a prompt. Services do not log either.
