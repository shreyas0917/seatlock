#!/usr/bin/env bash
# loadtest/poolstats.sh — the bottleneck measurement: runs capacity.js at
# RATE actions/s, RUNS times, and after each run prints what the DB pool did
# during it (Prometheus increase() over the run), next to the latency.
#
#   RATE=600 RUNS=3 loadtest/poolstats.sh
#
# Per run: HTTP requests, pool acquires, acquires per request (= DB round
# trips that needed a connection), the average number of requests WAITING
# for a connection (acquire wait seconds per second, Little's law), the
# share of acquires that had to wait, and Postgres' peak CPU.
set -u
cd "$(dirname "$0")/.." || exit 1
# shellcheck source=../demo/lib.sh
source demo/lib.sh

RATE="${RATE:-600}" RUNS="${RUNS:-3}" DURATION="${CAP_DURATION:-60s}"
BASE_DIR="${OUT_DIR:-tmp/loadtest/poolstats-$RATE-$(date +%Y%m%d-%H%M%S)}"
prom_at() { curl -fsS -G "$PROM_URL/api/v1/query" --data-urlencode "query=$1" --data-urlencode "time=$2" \
  | grep -oE '"value":\[[0-9.e+]+,"[^"]*"\]' | head -n 1 | sed -E 's/.*,"([^"]*)"\]/\1/'; }

echo "| Run | RPS | p95 hold (ms) | p95 seat map (ms) | p95 confirm (ms) | Pool acquires per request | Avg requests waiting for a connection | Acquires that waited | Postgres peak CPU |" | tee "$BASE_DIR.md"
echo "|---|---|---|---|---|---|---|---|---|" | tee -a "$BASE_DIR.md"
for i in $(seq 1 "$RUNS"); do
  RUNS=1 OUT_DIR="$BASE_DIR/run$i" K6_ENV="CAP_RATE=$RATE CAP_DURATION=$DURATION" ./loadtest/run.sh capacity > "$BASE_DIR-run$i.log" 2>&1
  R="$BASE_DIR/run$i/run1"
  # The measured window: the k6 run (from its wall-clock line), minus 5 s at each edge.
  secs="${DURATION%s}"
  end="$(date -r "$R.json" +%s)"; at=$((end - 5)); win=$((secs - 10))
  req="$(prom_at "sum(increase(seatlock_http_requests_total{job=\"api\"}[${win}s]))" "$at")"
  acq="$(prom_at "sum(increase(seatlock_db_pool_acquires_total{job=\"api\"}[${win}s]))" "$at")"
  empty="$(prom_at "sum(increase(seatlock_db_pool_empty_acquires_total{job=\"api\"}[${win}s]))" "$at")"
  wait="$(prom_at "sum(rate(seatlock_db_pool_acquire_wait_seconds_total{job=\"api\"}[${win}s]))" "$at")"
  pg="$(grep -E 'seatlock-postgres' "$R.peaks.txt" | grep -oE 'peak CPU +[0-9.]+' | awk '{print $3}')"
  g() { go run ./loadtest/report -get "$1" "$R.result.json"; }
  printf '| %s | %.0f | %.1f | %.1f | %.1f | %.2f | %.2f | %.1f%% | %s%% |\n' "$i" "$(g rps)" "$(g endpoints.hold.p95_ms)" \
    "$(g endpoints.seats.p95_ms)" "$(g endpoints.confirm.p95_ms)" "$(awk -v a="$acq" -v r="$req" 'BEGIN { print a / r }')" \
    "$wait" "$(awk -v e="$empty" -v a="$acq" 'BEGIN { print 100 * e / a }')" "$pg" | tee -a "$BASE_DIR.md"
  grep -q PASS "$R.verdict" || echo "  (run $i: correctness checks FAILED, see $BASE_DIR-run$i.log)"
done
