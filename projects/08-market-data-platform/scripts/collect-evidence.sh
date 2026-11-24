#!/bin/sh
# Regenerates docs/evidence/ by running every check and saving its output.
#
#   ./scripts/collect-evidence.sh            everything (needs Docker and .env)
#   ./scripts/collect-evidence.sh hermetic   only the part that needs no Docker
#
# Every file starts with what was run, when, and on what. A step that fails
# is recorded as failed in its file and makes this script exit non-zero; the
# remaining steps still run. Nothing here prints or stores the ClickHouse
# password: SQL runs inside the ClickHouse container with its own credentials.
#
# The Docker part starts this project's compose services, deletes this
# project's volumes first so the failure demo starts from empty stores, and
# stops the services when it is done.
set -u
cd "$(dirname "$0")/.."

MODE=${1:-all}
OUT=docs/evidence
BIN=bin/mdp
mkdir -p "$OUT"
failed=""

machine() {
  case "$(uname -s)" in
    Darwin) printf '%s, %s logical CPUs, %s GiB RAM' "$(sysctl -n machdep.cpu.brand_string)" "$(sysctl -n hw.ncpu)" "$(($(sysctl -n hw.memsize) / 1073741824))" ;;
    *) printf '%s, %s logical CPUs' "$(uname -m)" "$(getconf _NPROCESSORS_ONLN)" ;;
  esac
}

header() { # header <title> <command as documented>
  printf '# %s\n# captured %s on %s/%s, %s\n# machine: %s\n# command: %s\n\n' \
    "$1" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$(go env GOOS)" "$(go env GOARCH)" "$(go env GOVERSION)" "$(machine)" "$2"
}

# step <file> <title> <documented command> <shell snippet>
step() {
  file="$OUT/$1"
  printf '%-34s ' "$1"
  (
    header "$2" "$3"
    sh -c "$4" 2>&1
    status=$?
    printf '\nexit status: %s\n' "$status"
    exit "$status"
  ) > "$file"
  if [ $? -eq 0 ]; then echo ok; else echo FAILED; failed="$failed $1"; fi
}

# Keeps test result lines and drops the per-test log chatter.
RESULTS='grep -E "^(--- |ok|FAIL|PASS|panic:|[[:space:]]+--- |[[:space:]]+[a-z_]+_test\.go)"'

go build -o "$BIN" ./src/cmd/mdp || exit 1

step lint.txt "Lint: gofmt, go vet, staticcheck" "make lint" \
  "make lint"

step hermetic-tests.txt "Unit and scenario tests, race detector on (no Docker)" \
  "go test -race -count=1 -v ./tests/unit/... ./tests/scenarios/...   (make test, verbose)" \
  "go test -race -count=1 -v ./tests/unit/... ./tests/scenarios/... > $OUT/.raw 2>&1; s=\$?; $RESULTS $OUT/.raw; rm -f $OUT/.raw; exit \$s"

step fuzz.txt "Fuzzing, 30 s per target" \
  "go test -run '^\$' -fuzz <target> -fuzztime 30s ./tests/unit/" \
  "s=0; for f in FuzzUnitsNeverRounds FuzzParsersNeverPanic; do go test -run '^\$' -fuzz \"^\$f\\\$\" -fuzztime 30s ./tests/unit/ > $OUT/.raw 2>&1 || s=1; grep -Ev '^fuzz: elapsed: ([1-9]|[12][0-9])s,' $OUT/.raw; done; rm -f $OUT/.raw; exit \$s"

step coverage.txt "Statement coverage of src/ by the unit and scenario suites" \
  "go test -count=1 -coverpkg=./src/... -coverprofile=cover.out ./tests/unit/... ./tests/scenarios/... && go tool cover -func=cover.out" \
  "go test -count=1 -coverpkg=./src/... -coverprofile=$OUT/.cover ./tests/unit/... ./tests/scenarios/... | grep -E '^(ok|FAIL)' || exit 1
   echo
   echo 'Per file: mean of the per-function percentages. The Kafka and ClickHouse adapters show 0 here'
   echo 'because only the integration suite runs them. The last line is the exact statement total.'
   echo
   go tool cover -func=$OUT/.cover | awk '\$1 != \"total:\" {f=\$1; sub(/:[0-9]+:\$/, \"\", f); p=\$NF; sub(/%/, \"\", p); n[f]++; sum[f]+=p} END {for (f in n) printf \"%-36s %5.1f%%  (%d functions)\\n\", f, sum[f]/n[f], n[f]}' | sort
   echo
   go tool cover -func=$OUT/.cover | tail -1
   rm -f $OUT/.cover"

step replay.txt "Replay of the committed capture, twice, then regeneration of the fixtures" \
  "make replay; make replay; make fixtures" \
  "$BIN replay || exit 1
   echo; echo '## second run'
   $BIN replay || exit 1
   echo; echo '## regenerating the fixtures'
   before=\$(cat examples/fixtures/* | shasum -a 256)
   $BIN fixture || exit 1
   after=\$(cat examples/fixtures/* | shasum -a 256)
   [ \"\$before\" = \"\$after\" ] && echo 'fixtures are byte-identical after regeneration' || { echo 'fixtures CHANGED'; exit 1; }"

step demo.txt "Scripted walkthrough: in-process pipeline, simulated data, injected failures" "make demo" \
  "$BIN demo"

step bench-memory.txt "Load test on the in-memory log and store" \
  "make bench   (mdp bench --rate 2000 --duration 10s --backlog 50000)" \
  "$BIN bench"

if [ "$MODE" = hermetic ]; then
  [ -z "$failed" ] || { echo "failed:$failed"; exit 1; }
  exit 0
fi

[ -f .env ] || { echo 'no .env: run "make setup" first'; exit 1; }
DC="docker compose --profile app"
# Inside the ClickHouse container, as the user the container was created with.
CH="$DC exec -T clickhouse sh -c 'clickhouse-client --user \"\$CLICKHOUSE_USER\" --password \"\$CLICKHOUSE_PASSWORD\" --multiquery --echo --query \"\$0\"'"

$DC down -v --remove-orphans > /dev/null 2>&1
make deps-up > /dev/null 2>&1 || { echo "could not start Redpanda and ClickHouse"; exit 1; }
versions="$($DC exec -T redpanda rpk version 2>/dev/null | head -1); ClickHouse $($DC exec -T clickhouse clickhouse-client --version 2>/dev/null | awk '{print $4}')"

step integration-tests.txt "Integration tests against Redpanda and ClickHouse, race detector on" \
  "make integration-test   [$versions]" \
  "make integration-test > $OUT/.raw 2>&1; s=\$?; $RESULTS $OUT/.raw; rm -f $OUT/.raw; exit \$s"

ROWS="('now', now64(9)), ('old-200d', now64(9) - INTERVAL 200 DAY), ('year-1969', '1969-12-31 23:59:59'), ('year-2099', '2099-12-31 23:59:59'), ('year-2107', '2107-01-01 00:00:00'), ('year-2150', '2150-06-01 00:00:00')"
TABLE="(id String, event_ts DateTime64(9, 'UTC')) ENGINE = ReplacingMergeTree PARTITION BY toDate(event_ts) ORDER BY (id, event_ts)"
step clickhouse-timestamp-range.txt "ClickHouse and timestamps outside its 32-bit DateTime / 16-bit Date ranges" \
  "see below; throwaway database mdp_tmp_probe   [$versions]" \
  "echo 'Six rows go into two tables that differ only in the TTL expression. Both have the'
   echo 'platform engine, partitioning and a 90-day retention. Every INSERT is accepted.'
   echo; echo '## A. TTL through a 32-bit DateTime (the schema this project first used)'; echo
   $CH \"CREATE DATABASE IF NOT EXISTS mdp_tmp_probe;
CREATE TABLE mdp_tmp_probe.a $TABLE TTL toDateTime(event_ts) + INTERVAL 90 DAY;
INSERT INTO mdp_tmp_probe.a SETTINGS async_insert=0 VALUES $ROWS;
OPTIMIZE TABLE mdp_tmp_probe.a FINAL;
SELECT 'A, after a merge' AS table_, id, event_ts FROM mdp_tmp_probe.a ORDER BY id FORMAT PrettyCompactMonoBlock;\" || exit 1
   echo; echo '## B. TTL on the 64-bit column (the schema in src/storage/clickhouse.go)'; echo
   $CH \"CREATE TABLE mdp_tmp_probe.b $TABLE TTL event_ts + INTERVAL 90 DAY;
INSERT INTO mdp_tmp_probe.b SETTINGS async_insert=0 VALUES $ROWS;
OPTIMIZE TABLE mdp_tmp_probe.b FINAL;
SELECT 'B, after a merge' AS table_, id, event_ts, toDate(event_ts) AS partition FROM mdp_tmp_probe.b ORDER BY id FORMAT PrettyCompactMonoBlock;\" || exit 1
   echo; echo '## C. What the two conversions return for these values'; echo
   $CH \"SELECT ts, toDateTime(ts) AS as_32bit_datetime, toDate(ts) AS as_16bit_date FROM (SELECT arrayJoin([toDateTime64('1969-12-31 23:59:59', 9, 'UTC'), toDateTime64('2099-12-31 23:59:59', 9, 'UTC'), toDateTime64('2107-01-01 00:00:00', 9, 'UTC'), toDateTime64('2150-06-01 00:00:00', 9, 'UTC')]) AS ts) FORMAT PrettyCompactMonoBlock;
DROP DATABASE mdp_tmp_probe;\""

step bench-real.txt "Load test through Redpanda and ClickHouse in Docker on the same machine" \
  "make bench-real   (mdp bench --infra real --rate 2000 --duration 10s --backlog 50000)   [$versions]" \
  "make bench-real"

if make stack-up > "$OUT/.stack" 2>&1; then
  step failure-demo.txt "Failure demo against the compose stack, started from empty volumes" \
    "make clean && make stack-up && make failure-demo   [$versions]" \
    "make failure-demo"
else
  echo "failure-demo.txt                   FAILED (stack did not start; see $OUT/.stack)"
  failed="$failed failure-demo.txt"
fi
[ -z "$failed" ] && rm -f "$OUT/.stack"
make stack-down > /dev/null 2>&1

[ -z "$failed" ] || { echo "failed:$failed"; exit 1; }
echo "evidence written to $OUT/"
