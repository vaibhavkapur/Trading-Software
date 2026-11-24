#!/bin/sh
# Walk through the HTTP API against a local server on a manual simulated clock.
# All activity is SIMULATED / PAPER. Requires curl; run `make build` first.
set -eu
addr="127.0.0.1:${PORT:-8089}"
./bin/sor serve -addr "$addr" -speed 0 &
pid=$!
trap 'kill $pid 2>/dev/null' EXIT
sleep 1
call() { set -- "$1" "$2" "${3:-}"; echo; echo "\$ $1 $2 ${3:-}"; curl -sS -X "$1" "http://$addr$2" ${3:+-d "$3"}; }
call POST /sim/advance '{"ms":1000}'
call POST /parents '{"symbol":"XYZ","side":"BUY","qty":10000,"strategy":"TWAP","duration_seconds":600}'
call POST /sim/advance '{"ms":120000}'
call POST /sim/venues/BRAVO/faults '{"latency_ms":200,"reject_rate":0.1}'
call POST /sim/venues/ALPHA/faults '{"down":true}'
call POST /sim/advance '{"ms":481000}'
call GET /parents/P-000001
call GET /parents/P-000001/execution-quality
call GET /metrics
echo
