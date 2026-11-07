#!/usr/bin/env bash
# chaos/lib.sh — shared by the chaos scripts. Each script:
#   1. starts background load: loadtest/capacity.js at CHAOS_RATE actions/s
#      (default 200, about 50% of the measured knee) for CHAOS_DURATION;
#   2. breaks one thing (Redis, an api replica, the worker, the gateway,
#      Postgres), records each event, and measures recovery;
#   3. after the load: prints a Prometheus timeline (what users saw),
#      the k6 summary, then the correctness checks of loadtest/k6lib.sh:
#      time to consistency, bookings, 0 double charges, verify.sql 0 rows,
#      invariant gauges 0. Exit code 0 only if everything passed.
# Needs `make up-bench` (or `make up-scale` for api-replica.sh) and `make seed-load`.
set -u
cd "$(dirname "$0")/.." || exit 1
# shellcheck source=../demo/lib.sh
source demo/lib.sh
# shellcheck source=../loadtest/k6lib.sh
source loadtest/k6lib.sh

CHAOS_RATE="${CHAOS_RATE:-200}"
CHAOS_DURATION="${CHAOS_DURATION:-150s}"
NAME="$(basename "$0" .sh)"
OUT_DIR="${OUT_DIR:-tmp/chaos/$NAME-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$OUT_DIR"
EVENTS="$OUT_DIR/events.txt"
: > "$EVENTS"
LOAD_PID="" LOAD_START=""

# event LABEL - records "now: label" (shown in the timeline) and prints it.
event() {
  echo "$(date +%s) $*" >> "$EVENTS"
  echo "  [t+$(($(date +%s) - LOAD_START))s] $*"
}

# start_load [extra K6_ENV] - background capacity.js load, summary in $OUT_DIR/load.json
start_load() {
  echo "=== chaos/$NAME: load ${CHAOS_RATE} actions/s for $CHAOS_DURATION (BASE_URL=$BASE_URL)"
  ./loadtest/env.sh > "$OUT_DIR/env.txt"
  reset_all
  # The rush show's seat ids, looked up ONCE (a lookup per curl call would
  # slow the scripted steps down by a second each).
  RUSH_SHOW="$(sql "SELECT id FROM shows WHERE title = 'Load: Rush'" | tr -d '[:space:]')"
  RUSH_SEATS=($(sql "SELECT string_agg(id::text, ' ' ORDER BY id) FROM seats WHERE show_id = $RUSH_SHOW" | tr -d '\r\n'))
  K6_ENV="CAP_RATE=$CHAOS_RATE CAP_DURATION=$CHAOS_DURATION ${1:-}" K6_NAME=seatlock-k6-chaos \
    k6 capacity.js "$OUT_DIR/load.json" > "$OUT_DIR/load.k6.log" 2>&1 &
  LOAD_PID=$!
  LOAD_START=$(date +%s)
  : > "$OUT_DIR/stats.csv"
  sample_stats "$OUT_DIR/stats.csv" &
  SAMPLER=$!
  event "load started"
}

# wait_until_t SECONDS - sleeps until SECONDS after the load started.
wait_until_t() {
  local left=$(($1 - ($(date +%s) - LOAD_START)))
  [ "$left" -gt 0 ] && sleep "$left"
}

# finish_load - waits for k6, prints the timeline and the k6 summary.
finish_load() {
  wait "$LOAD_PID"
  local rc=$?
  kill "$SAMPLER" 2> /dev/null; wait "$SAMPLER" 2> /dev/null
  local end
  end=$(date +%s)
  event "load finished (k6 exit $rc)"
  echo
  echo "--- timeline (Prometheus, 10 s steps; what users saw)"
  sleep 6 # one more scrape
  go run ./loadtest/report -timeline "$PROM_URL" -from "$((LOAD_START - 10))" -to "$((end + 10))" -zero "$LOAD_START" -events "$EVENTS" | tee "$OUT_DIR/timeline.txt"
  echo
  echo "--- k6 summary (all statuses, whole run)"
  go run ./loadtest/report -duration "$CHAOS_DURATION" "$OUT_DIR/load.json" | tee "$OUT_DIR/load.summary.txt"
  s500="$(result_get "$OUT_DIR/load.result.json" counters.status_500)"
  if [ "${s500:-0}" = "0" ]; then pass "no HTTP 500 responses (k6)"; else fail "$s500 HTTP 500 responses: a bug"; fi
}

# api_hold SHOW_TITLE SEAT_INDEX USER - POST /v1/holds via $API_URL; prints "STATUS BODY".
# rush_seat_id N - the id of the Nth seat (0-based) of the rush show.
rush_seat_id() { echo "${RUSH_SEATS[$1]}"; }
api_hold() { # SEAT_ID USER
  curl -s -m 10 -o "$OUT_DIR/.body" -w '%{http_code}' -X POST "$API_URL/v1/holds" \
    -H 'Content-Type: application/json' -H "X-User-ID: $2" -d "{\"show_id\":$RUSH_SHOW,\"seat_id\":$1}"
  echo " $(cat "$OUT_DIR/.body" 2> /dev/null)"
}
api_confirm() { # HOLD_ID USER KEY
  curl -s -m 15 -o "$OUT_DIR/.body" -w '%{http_code}' -X POST "$API_URL/v1/bookings" \
    -H 'Content-Type: application/json' -H "X-User-ID: $2" -H "Idempotency-Key: $3" -d "{\"hold_id\":$1}"
  echo " $(cat "$OUT_DIR/.body" 2> /dev/null)"
}

# alert_fired NAME SINCE_EPOCH - 1 if the alert was FIRING at any time since then.
alert_fired() {
  local range=$(($(date +%s) - $2 + 5))
  local v
  v="$(prom_value "max_over_time(ALERTS{alertname=\"$1\",alertstate=\"firing\"}[${range}s])")"
  [ "${v%%.*}" = "1" ]
}

# summary_line - the one-line result for RESULTS.md's chaos table.
chaos_done() {
  echo
  peaks "$OUT_DIR/stats.csv" > "$OUT_DIR/peaks.txt"
  if [ "$FAILURES" -eq 0 ]; then
    echo "RESULT: PASS  (chaos/$NAME, logs in $OUT_DIR)"
    exit 0
  fi
  echo "RESULT: FAIL ($FAILURES)  (chaos/$NAME, logs in $OUT_DIR)"
  exit 1
}
