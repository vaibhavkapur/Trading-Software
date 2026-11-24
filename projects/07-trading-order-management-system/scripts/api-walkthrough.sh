#!/bin/sh
# Walk through the HTTP API of a running stack with curl, including one
# injected failure (the broker's webhooks are lost) and its repair.
# SIMULATED / PAPER ONLY.
#
# Works against `make run` (credentials in .local/credentials.env) or
# `make up` (credentials in .env). Override with CREDENTIALS=path.
# Tokens are read from that file and never echoed.
set -eu
cd "$(dirname "$0")/.."
creds="${CREDENTIALS:-}"
if [ -z "$creds" ]; then
	if [ -f .local/credentials.env ]; then creds=.local/credentials.env; else creds=.env; fi
fi
[ -f "$creds" ] || { echo "walkthrough: no credentials file ($creds); start the stack with 'make run' or 'make env && make up'" >&2; exit 1; }
set -a; . "./$creds"; set +a
: "${OMS_URL:?}" "${BROKER_URL:?}" "${TRADER_TOKEN:?}" "${OPERATOR_TOKEN:?}" "${OMS_BROKER_API_KEY:?}"
command -v curl >/dev/null 2>&1 || { echo "walkthrough: curl is required" >&2; exit 1; }

say() { printf '\n== %s\n' "$*"; }
# api <who> <token> <method> <path> [body] [idempotency-key]
api() {
	who="$1"; token="$2"; method="$3"; path="$4"; body="${5:-}"; key="${6:-}"
	printf '$ curl -X %s %s%s   (as %s%s)\n' "$method" "$OMS_URL" "$path" "$who" "${key:+, Idempotency-Key: $key}" >&2
	curl -sS -X "$method" "$OMS_URL$path" -H "Authorization: Bearer $token" \
		${body:+-H "Content-Type: application/json" -d "$body"} ${key:+-H "Idempotency-Key: $key"}
}
sim() { # sim <method> <path> [body]
	printf '$ curl -X %s %s%s   (simulator control)\n' "$1" "$BROKER_URL" "$2" >&2
	curl -sS -X "$1" "$BROKER_URL$2" -H "Authorization: Bearer $OMS_BROKER_API_KEY" ${3:+-H "Content-Type: application/json" -d "$3"}
}
order_id() { sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -n 1; }

run="walk-$(date +%s)"
order='{"account_id":"ACC-1","symbol":"AAPL","side":"BUY","type":"LIMIT","time_in_force":"DAY","qty":100,"limit_price":"150.00"}'

say "Health (no credentials needed)"
curl -sS "$OMS_URL/healthz"
curl -sS "$OMS_URL/readyz"

say "Create an order, then repeat the identical request: one order, same response"
first="$(api alice "$TRADER_TOKEN" POST /v1/orders "$order" "$run-1")"; echo "$first"
again="$(api alice "$TRADER_TOKEN" POST /v1/orders "$order" "$run-1")"
id="$(echo "$first" | order_id)"
[ "$first" = "$again" ] && echo "(the repeat returned the identical body for order $id)" || { echo "the repeat differed" >&2; exit 1; }

say "The broker acknowledges and fills it asynchronously; read it back"
sleep 3
api alice "$TRADER_TOKEN" GET "/v1/orders/$id"
api alice "$TRADER_TOKEN" GET "/v1/orders/$id/events"
api alice "$TRADER_TOKEN" GET "/v1/positions?account_id=ACC-1"

say "Failure: the broker acts but its webhooks are lost"
sim PUT /sim/faults '{"drop_webhooks":true}'
second="$(api alice "$TRADER_TOKEN" POST /v1/orders "$order" "$run-2")"
id2="$(echo "$second" | order_id)"
sleep 3
sim PUT /sim/faults '{}'
echo "The OMS has heard nothing about $id2 (the broker has filled it):"
api alice "$TRADER_TOKEN" GET "/v1/orders/$id2"

say "Reconciliation compares with the broker's books and repairs the drift (after the 5s grace window)"
sleep 6
api olivia "$OPERATOR_TOKEN" POST /v1/reconciliation/runs
api alice "$TRADER_TOKEN" GET "/v1/orders/$id2"
api olivia "$OPERATOR_TOKEN" GET "/v1/reconciliation/issues?status=OPEN"

say "A trader cannot run reconciliation"
api alice "$TRADER_TOKEN" POST /v1/reconciliation/runs

say "Metrics (excerpt)"
curl -sS "$OMS_URL/metrics" | grep -E '^oms_(orders_created|idempotent_replays|broker_events|reconciliation_(drift|auto_repaired)|outbox_pending)' || true
