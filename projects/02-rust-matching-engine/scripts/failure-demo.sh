#!/usr/bin/env bash
# Failure demo: the gateway process is killed with SIGKILL mid-session and a
# half-written record is left in the command log. On restart the engine
# recovers from its journal and the resting order is still there.
# SIMULATED DATA ONLY.
set -euo pipefail
cd "$(dirname "$0")/.."

PORT="${PORT:-9917}"
banner() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }
WORK="$(mktemp -d)"
PID=""
cleanup() { [ -n "$PID" ] && kill -9 "$PID" 2>/dev/null || true; rm -rf "$WORK"; }
trap cleanup EXIT

cargo build --locked -q
ME=target/debug/me

start() {
  "$ME" serve --addr "127.0.0.1:$PORT" --data-dir "$WORK/data" 2> "$WORK/server.log" &
  PID=$!
  for _ in $(seq 1 100); do
    if grep -q "listening" "$WORK/server.log" 2>/dev/null; then return 0; fi
    sleep 0.05
  done
  echo "gateway did not start:"; cat "$WORK/server.log"; exit 1
}
send() { printf '%s\n' "$1" | nc -w 2 127.0.0.1 "$PORT"; }

banner "SIMULATED / PAPER TRADING - failure demo"

banner "1. Start gateway, rest SELL 50 @ 200, fill 20 of it"
start
send '{"type":"NEW","order_id":1,"symbol":"XYZ","side":"SELL","qty":50,"price_ticks":200}'
send '{"type":"NEW","order_id":2,"symbol":"XYZ","side":"BUY","qty":20,"price_ticks":200}'

banner "2. kill -9 the gateway (no shutdown hook, no snapshot)"
kill -9 "$PID"; wait "$PID" 2>/dev/null || true; PID=""
echo "gateway is gone; a client now gets:"
send '{"type":"CANCEL","order_id":1}' || echo "  connection refused (as expected)"

banner "3. Simulate a record torn by the crash"
printf '{"seq":3,"ts_ns":0,"cmd":{"type":"NEW","order_' >> "$WORK/data/commands.jsonl"
tail -c 120 "$WORK/data/commands.jsonl"; echo "   <- no newline: torn"

banner "4. Offline recovery report"
"$ME" recover --data-dir "$WORK/data"

banner "5. Restart: the remaining 30 lots are still resting and still trade"
start
grep -E "recovered|XYZ" "$WORK/server.log"
send '{"type":"NEW","order_id":3,"symbol":"XYZ","side":"BUY","ord_type":"MARKET","qty":100}'

banner "6. Clean shutdown (SIGTERM) writes a snapshot"
kill -TERM "$PID"; wait "$PID" 2>/dev/null || true; PID=""
tail -n 3 "$WORK/server.log"
ls "$WORK/data" "$WORK/data/snapshots"

banner "Done (simulated)"
