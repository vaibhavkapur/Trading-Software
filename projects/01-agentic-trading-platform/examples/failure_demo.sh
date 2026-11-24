#!/usr/bin/env bash
# Failure demo against a running dev server: approve an order, break the broker's order
# entry, watch the submit fail safely, restore it, and retry. PAPER trading only.
set -euo pipefail

BASE="${ATP_BASE_URL:-http://127.0.0.1:8000}"
KEY="${ATP_API_KEY:-dev-local-key}"
field() { python3 -c "import sys, json; print(json.load(sys.stdin)$1)"; }
call() { curl -sS -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" "$@"; }

CREATED=$(call -X POST "$BASE/v1/intents" -d '{"text": "Buy 5 shares of QQQ", "mode": "paper"}')
PROPOSAL=$(echo "$CREATED" | field '["proposal"]["id"]')
HASH=$(echo "$CREATED" | field '["proposal"]["proposal_hash"]')
call -X POST "$BASE/v1/proposals/$PROPOSAL/evaluate" >/dev/null
ORDER=$(call -X POST "$BASE/v1/proposals/$PROPOSAL/approve" \
  -d "{\"proposal_hash\": \"$HASH\", \"decision\": \"approve\"}" | field '["order_id"]')

echo "== broker order entry goes down"
call -X POST "$BASE/v1/sim/faults" -d '{"broker_order_entry_down": true}' >/dev/null
call -X POST "$BASE/v1/orders/$ORDER/submit" -w '\nHTTP %{http_code}\n'

echo "== broker recovers; retry the same order id"
call -X POST "$BASE/v1/sim/faults" -d '{"broker_order_entry_down": false}' >/dev/null
call -X POST "$BASE/v1/orders/$ORDER/submit" | field '["order"]["state"]'
