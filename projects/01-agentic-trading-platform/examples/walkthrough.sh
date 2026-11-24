#!/usr/bin/env bash
# Happy-path walkthrough against a running server (`make run`). PAPER trading only.
# Usage: examples/walkthrough.sh ["Buy \$500 of SPY using a limit order"]
set -euo pipefail

BASE="${ATP_BASE_URL:-http://127.0.0.1:8000}"
KEY="${ATP_API_KEY:-dev-local-key}"   # dev default; set ATP_API_KEY for anything else
TEXT="${1:-Buy \$500 of SPY using a limit order}"
field() { python3 -c "import sys, json; print(json.load(sys.stdin)$1)"; }
call() { curl -sS -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" "$@"; }

echo "== intent: $TEXT"
BODY=$(python3 -c 'import json, sys; print(json.dumps({"text": sys.argv[1], "mode": "paper"}))' "$TEXT")
CREATED=$(call -X POST "$BASE/v1/intents" -d "$BODY")
PROPOSAL=$(echo "$CREATED" | field '["proposal"]["id"]')
HASH=$(echo "$CREATED" | field '["proposal"]["proposal_hash"]')
CID=$(echo "$CREATED" | field '["correlation_id"]')
echo "$CREATED" | python3 -m json.tool

echo "== risk evaluation"
call -X POST "$BASE/v1/proposals/$PROPOSAL/evaluate" | field '["risk_decision"]["violations"]'

echo "== approve (bound to $HASH)"
ORDER=$(call -X POST "$BASE/v1/proposals/$PROPOSAL/approve" \
  -d "{\"proposal_hash\": \"$HASH\", \"decision\": \"approve\"}" | field '["order_id"]')

echo "== submit $ORDER (twice: the second is a no-op)"
call -X POST "$BASE/v1/orders/$ORDER/submit" | field '["order"]["state"]'
call -X POST "$BASE/v1/orders/$ORDER/submit" | field '["order"]["state"]'

echo "== portfolio"
call "$BASE/v1/portfolio" | python3 -m json.tool

echo "== audit trail for $CID"
call "$BASE/v1/audit?correlation_id=$CID" |
  python3 -c 'import sys, json
for e in json.load(sys.stdin)["events"]:
    print(e["seq"], e["ts"], e["actor"], e["event_type"])'
