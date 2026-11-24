#!/bin/sh
# Failure demo against the running compose stack (`make stack-up`).
#
# It breaks one thing at a time (sink, normalizer, ClickHouse, the venues,
# the broker, the ingestors) and after each one audits ClickHouse:
#
#   for every provider sequence domain, each sequence number between the
#   first and the last one stored is either stored exactly once or covered
#   by a declared gap.
#
# The audit pauses the SIMULATED venues and waits for the pipeline to drain
# first, so it compares a settled state. It exits non-zero on any mismatch.
#
# Everything here acts only on this project's compose services. No password
# appears on a command line: ClickHouse is queried from inside its container,
# which already holds its own credentials.
set -eu
cd "$(dirname "$0")/.."

# Optional port overrides live in .env; compose reads it for the rest.
if [ -f .env ]; then set -a; . ./.env; set +a; fi
GATEWAY="http://127.0.0.1:${MDP_GATEWAY_PORT:-8080}"
SIM="http://127.0.0.1:${MDP_SIM_ADMIN_PORT:-9103}"
PROM="http://127.0.0.1:${MDP_PROMETHEUS_PORT:-9091}"
DC="docker compose --profile app"

say()  { printf '\n== %s\n' "$*"; }
note() { printf '   %s\n' "$*"; }
die()  { printf '   FAIL  %s\n' "$*" >&2; exit 1; }

ch() { # run SQL inside the ClickHouse container
  $DC exec -T clickhouse sh -c 'clickhouse-client --user "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD" --database "$CLICKHOUSE_DB" --query "$0"' "$1"
}

lag() { # committed lag of a consumer group, in records
  $DC exec -T redpanda rpk group describe "$1" 2>/dev/null | awk '/^TOTAL-LAG/ {print $2}'
}

fault() { # fault <venue> <fault> [query]
  curl -fsS -o /dev/null -X POST "$SIM/faults/$1/$2?${3:-}" || die "simulator admin API not reachable at $SIM"
}

wait_for() { # wait_for <seconds> <description> <command...>
  limit=$1; what=$2; shift 2
  i=0
  until "$@" >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -le "$limit" ] || die "timed out after ${limit}s waiting for $what"
    sleep 1
  done
}

drained() { [ "$(lag mdp-normalizer)" = "0" ] && [ "$(lag mdp-sink)" = "0" ]; }
gateway_up() { curl -fsS -o /dev/null "$GATEWAY/healthz"; }
store_ok() { curl -fsS "$GATEWAY/v1/health" | grep -q '"store":{"ok":true'; }
both_connected() { [ "$(curl -fsS "$GATEWAY/v1/health" | grep -o '"connected":true' | wc -l | tr -d ' ')" = "2" ]; }

# Whatever happens, do not leave the venues paused or a service stopped.
restore() {
  curl -s -o /dev/null -X POST "$SIM/faults/alpha/resume" || true
  curl -s -o /dev/null -X POST "$SIM/faults/beta/resume" || true
  $DC up -d >/dev/null 2>&1 || true
}
trap restore EXIT

AUDIT_FROM="
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
  GROUP BY provider, session, seq_domain),
audit AS (
  SELECT p.provider AS provider, p.session AS session, p.seq_domain AS seq_domain, p.lo AS first_seq, p.hi AS last_seq,
         p.stored AS stored, (p.hi - p.lo + 1) - p.stored AS absent, d.missing AS declared_missing
  FROM present AS p LEFT JOIN declared AS d ON d.provider = p.provider AND d.session = p.session AND d.seq_domain = p.seq_domain)"

audit() { # audit <label>
  fault alpha pause; fault beta pause
  wait_for 180 "the pipeline to drain" drained
  # Offsets are committed just after the rows are written; give the last batch a moment.
  sleep 1
  ch "$AUDIT_FROM SELECT * FROM audit ORDER BY provider, session, seq_domain FORMAT PrettyCompactMonoBlock" | sed 's/^/   /'
  bad=$(ch "$AUDIT_FROM SELECT countIf(absent != declared_missing) FROM audit")
  domains=$(ch "$AUDIT_FROM SELECT count() FROM audit")
  logical=$(ch "SELECT (SELECT count() FROM trades FINAL) + (SELECT count() FROM quotes FINAL) + (SELECT count() FROM depth FINAL)")
  physical=$(ch "SELECT (SELECT count() FROM trades) + (SELECT count() FROM quotes) + (SELECT count() FROM depth)")
  note "rows visible to queries: $logical; rows on disk before merges: $physical (the difference is redelivery, removed by the deduplication key)"
  fault alpha resume; fault beta resume
  [ "$domains" -gt 0 ] || die "$1: the store is empty"
  [ "$bad" = "0" ] || die "$1: $bad sequence domain(s) have sequence numbers that are neither stored nor declared missing"
  note "ok    $1: every sequence number is stored once or declared missing"
}

say "0. Preflight"
gateway_up || die "gateway not reachable at $GATEWAY: run 'make stack-up' first"
wait_for 60 "both ingestors to be connected" both_connected
note "SIMULATED venues are publishing; gateway at $GATEWAY"
sleep 5
audit "baseline"

say "1. Sink stopped for 10 seconds"
$DC stop sink >/dev/null 2>&1
sleep 10
note "sink consumer lag while stopped: $(lag mdp-sink) records (the canonical topics keep the data)"
code=$(curl -s -o /dev/null -w '%{http_code}' "$GATEWAY/v1/trades?instrument_id=venueA:XYZ&limit=1")
note "historical queries still answer from what is stored: HTTP $code"
$DC start sink >/dev/null 2>&1
audit "after sink restart"

say "2. Normalizer killed (SIGKILL), restarted 5 seconds later"
$DC kill normalize >/dev/null 2>&1
sleep 5
note "normalizer consumer lag while dead: $(lag mdp-normalizer) records"
$DC start normalize >/dev/null 2>&1
audit "after normalizer crash"

say "3. ClickHouse stopped for about 15 seconds"
$DC stop clickhouse >/dev/null 2>&1
sleep 8
code=$(curl -s -o /dev/null -w '%{http_code}' "$GATEWAY/v1/trades?instrument_id=venueA:XYZ&limit=1")
note "GET /v1/trades during the outage: HTTP $code"
[ "$code" = "503" ] || die "expected 503 from the gateway while the store is down, got $code"
retrying() { curl -fsS "$PROM/api/v1/query?query=mdp_sink_retrying" | grep -q '"1"\]'; }
wait_for 30 "Prometheus to show mdp_sink_retrying = 1" retrying
note "Prometheus: mdp_sink_retrying = 1; sink consumer lag: $(lag mdp-sink) records"
$DC up -d --wait clickhouse >/dev/null 2>&1
wait_for 60 "the gateway to report the store healthy" store_ok
audit "after ClickHouse outage"

say "4. Venue beta loses 5 events for good; venue alpha drops its connection"
before=$(ch "SELECT count() FROM feed_gaps FINAL")
fault beta lose "n=5"
fault alpha disconnect
sleep 4
wait_for 60 "both ingestors to be connected again" both_connected
audit "after venue faults"
after=$(ch "SELECT count() FROM feed_gaps FINAL")
[ "$after" -gt "$before" ] || die "the lost events were not declared as a gap"
note "ok    declared gaps in the store: $before -> $after"
curl -fsS "$GATEWAY/v1/gaps?instrument_id=venueB:XYZ.B" | sed 's/^/   GET \/v1\/gaps -> /' | cut -c1-400

say "5. Broker (Redpanda) restarted"
$DC restart redpanda >/dev/null 2>&1
$DC up -d --wait redpanda >/dev/null 2>&1
wait_for 120 "both ingestors to be connected again" both_connected
sleep 5
audit "after broker restart"

say "6. Ingestors killed, restarted 5 seconds later"
$DC kill ingest >/dev/null 2>&1
sleep 5
$DC start ingest >/dev/null 2>&1
wait_for 60 "both ingestors to be connected again" both_connected
sleep 3
audit "after ingestor crash"

say "Result"
note "PASS: after every failure, the store holds each provider event once or a declared gap in its place."
