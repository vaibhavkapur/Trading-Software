#!/bin/sh
# Drives a running surveild with curl: load the example events, look at
# the alert, work it as a case, export the evidence.
#
# Start the server first (simulated data only):
#
#   export SURV_INGEST_TOKEN=$(openssl rand -hex 16)
#   export SURV_ADMIN_TOKEN=$(openssl rand -hex 16)
#   export ALICE_TOKEN=$(openssl rand -hex 16)
#   export SURV_ANALYST_TOKENS="alice:$ALICE_TOKEN"
#   make run
#
# then, in another shell with the same variables exported:
#
#   sh scripts/walkthrough.sh
#
# Tokens are read from the environment and passed to curl through a
# header file, so they do not show up in the process list or in the
# output. Needs curl and jq.
set -eu

URL="${SURV_URL:-http://127.0.0.1:8080}"
: "${SURV_INGEST_TOKEN:?export SURV_INGEST_TOKEN}"
: "${ALICE_TOKEN:?export ALICE_TOKEN (the token given to alice in SURV_ANALYST_TOKENS)}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
umask 077
printf 'Authorization: Bearer %s\n' "$SURV_INGEST_TOKEN" > "$tmp/ingest"
printf 'Authorization: Bearer %s\n' "$ALICE_TOKEN" > "$tmp/alice"

echo "== ingest examples/events.jsonl (11 events: a layering-like pattern)"
jq -s . examples/events.jsonl | curl -sS -H @"$tmp/ingest" --data-binary @- "$URL/v1/events" | jq -c .

echo "== declare the batch complete, so the last windows are evaluated"
curl -sS -X POST -H @"$tmp/ingest" "$URL/v1/ingest/flush" | jq -c .

echo "== open alerts in SIMA"
curl -sS -H @"$tmp/alice" "$URL/v1/alerts?symbol=SIMA&status=open" |
  jq -r '.alerts[] | "\(.id)  \(.rule_id) v\(.rule_version)  \(.severity)  \(.accounts | join(","))\n  \(.explanation)"'

alert=$(curl -sS -H @"$tmp/alice" "$URL/v1/alerts?symbol=SIMA&status=open&rule_id=LAYERING_LIKE" | jq -r '.alerts[0].id // empty')
if [ -z "$alert" ]; then
  echo "no open LAYERING_LIKE alert (already in a case from an earlier run?)"
  exit 0
fi

echo "== open a case on $alert"
case_id=$(curl -sS -H @"$tmp/alice" -d '{"note":"Picked up from the queue."}' "$URL/v1/alerts/$alert/cases" | jq -r .id)
echo "$case_id"

echo "== triage, annotate, resolve"
curl -sS -H @"$tmp/alice" -d '{"priority":"P2"}' "$URL/v1/cases/$case_id/triage" | jq -c '{status, priority, assignee}'
curl -sS -H @"$tmp/alice" -d '{"text":"Four sell orders pulled about a second after a small buy fill."}' "$URL/v1/cases/$case_id/notes" | jq -c '{notes: (.analyst_notes | length)}'
curl -sS -H @"$tmp/alice" -d '{"disposition":"INCONCLUSIVE","summary":"Single episode; nothing further to go on."}' "$URL/v1/cases/$case_id/resolve" | jq -c '{status, disposition}'

echo "== export the evidence bundle"
curl -sS -H @"$tmp/alice" "$URL/v1/cases/$case_id/evidence" |
  jq '{case: .case.id, exported_by, snapshots: (.evidence | length), events: (.evidence[0].events | length), audit_trail: [.audit_trail[] | "\(.seq) \(.action) by \(.actor)"], audit_chain, bundle_hash}'
