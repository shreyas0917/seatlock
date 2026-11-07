#!/usr/bin/env bash
# loadtest/run.sh — runs one k6 scenario the SAME way every time, then checks
# that SeatLock stayed correct.
#
#   loadtest/run.sh <scenario> [runs]       scenario = rush | capacity | spike | soak | smoke
#
#   RUNS default 1. Each run: reset data + gateway -> 15 s warm-up -> reset
#   again -> measured k6 run (k6 in Docker, INSIDE the Compose network, so the
#   Windows/macOS host port proxy is not what gets measured) -> checks
#   (loadtest/k6lib.sh correctness_checks):
#     1. time to consistency: 0 PENDING payments and 0 PENDING outbox rows
#     2. bookings in Postgres == bookings k6 saw (and == seats, for rush/smoke)
#     3. gateway: charges that took money == bookings + refunds, <= 1 per key
#     4. demo/verify.sql: every check 0 rows
#     5. Prometheus: a FRESH invariant run reports all 11 checks, max 0
#   plus k6's thresholds, no HTTP 500, and per scenario: spike recovery,
#   soak "nothing grows". It samples `docker stats` (k6 itself, api,
#   postgres, redis...) for the whole run: k6 shares the CPUs with the
#   system under test.
#
# Env: K6_ENV="CAP_RATE=400 X=1" (extra k6 -e vars), BASE_URL (default
# http://api:8080), OUT_DIR, SKIP_WARMUP=1, NETWORK (default seatlock_default).
# Needs the stack up (make up-bench) and the load data (make seed-load).
set -u
cd "$(dirname "$0")/.." || exit 1
# shellcheck source=../demo/lib.sh
source demo/lib.sh
# shellcheck source=k6lib.sh
source loadtest/k6lib.sh

SCENARIO="${1:?usage: loadtest/run.sh <rush|capacity|spike|soak|smoke> [runs]}"
RUNS="${2:-${RUNS:-1}}"

# Active duration of each scenario (for RPS) and the exact booking count, if any.
EXPECT_SHOW="" EXPECT_BOOKINGS="" STRICT=1
case "$SCENARIO" in
  rush) DURATION=10s; EXPECT_SHOW="Load: Rush"; EXPECT_BOOKINGS=100 ;;
  smoke) DURATION=10s; EXPECT_SHOW="Load: Smoke"; EXPECT_BOOKINGS=20 ;;
  capacity) DURATION="$(echo "$K6_ENV" | grep -oE 'CAP_DURATION=[^ ]+' | cut -d= -f2)"; DURATION="${DURATION:-60s}"; STRICT=0 ;;
  spike) DURATION=150s ;;
  soak) DURATION="$(echo "$K6_ENV" | grep -oE 'SOAK_DURATION=[^ ]+' | cut -d= -f2)"; DURATION="${DURATION:-10m}" ;;
  *) echo "unknown scenario $SCENARIO"; exit 2 ;;
esac

OUT_DIR="${OUT_DIR:-tmp/loadtest/$SCENARIO-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$OUT_DIR"

{
  echo "=== loadtest/run.sh $SCENARIO, $RUNS run(s), K6_ENV='$K6_ENV', BASE_URL=$BASE_URL, $(date)"
  ./loadtest/env.sh
} | tee "$OUT_DIR/env.txt"

for i in $(seq 1 "$RUNS"); do
  FAILURES=0
  RUN="$OUT_DIR/run$i"
  echo
  echo "=== run $i/$RUNS"
  reset_all
  if [ -z "${SKIP_WARMUP:-}" ]; then
    k6 warmup.js "" > "$RUN.warmup.log" 2>&1 || echo "  (warm-up exited non-zero, see $RUN.warmup.log)"
    reset_all
  fi
  sleep 2

  : > "$RUN.stats.csv"
  sample_stats "$RUN.stats.csv" &
  SAMPLER=$!
  start=$(date +%s)
  k6 "$SCENARIO.js" "$RUN.json" > "$RUN.k6.log" 2>&1
  k6_rc=$?
  kill "$SAMPLER" 2> /dev/null; wait "$SAMPLER" 2> /dev/null
  end=$(date +%s)
  echo "  k6 exit code $k6_rc ($((end - start))s wall clock), log $RUN.k6.log"
  if [ ! -s "$RUN.json" ]; then
    fail "k6 wrote no summary"; tail -20 "$RUN.k6.log"; echo FAIL > "$RUN.verdict"; continue
  fi
  go run ./loadtest/report -duration "$DURATION" "$RUN.json" | sed 's/^/  /'

  echo "  --- checks"
  correctness_checks "$RUN" "$EXPECT_SHOW" "$EXPECT_BOOKINGS"

  # spike: latency must come back to the baseline after the spike.
  if [ "$SCENARIO" = "spike" ]; then
    base_p95="$(result_get "$RUN.result.json" phases.baseline.endpoints.hold.p95_ms)"
    peak_p95="$(result_get "$RUN.result.json" phases.spike.endpoints.hold.p95_ms)"
    rec_p95="$(result_get "$RUN.result.json" phases.recovery.endpoints.hold.p95_ms)"
    # Recovered = recovery p95 <= 1.5 x baseline p95 + 5 ms (tiny numbers are noisy).
    if awk -v b="$base_p95" -v r="$rec_p95" 'BEGIN { exit !(b > 0 && r > 0 && r <= 1.5 * b + 5) }'; then
      pass "spike: hold p95 recovered (baseline ${base_p95} ms, spike ${peak_p95} ms, recovery ${rec_p95} ms)"
    else
      fail "spike: hold p95 did not recover (baseline '${base_p95}' ms, spike '${peak_p95}' ms, recovery '${rec_p95}' ms)"
    fi
  fi

  # soak: nothing may grow. Compare Prometheus averages over minutes 2-4 of
  # the run with the last 2 minutes (the first 2 minutes are warm-up).
  if [ "$SCENARIO" = "soak" ]; then
    early=$((start + 240)) late="$end"
    prom_at() { curl -fsS -G "$PROM_URL/api/v1/query" --data-urlencode "query=$1" --data-urlencode "time=$2" \
      | grep -oE '"value":\[[0-9.e+]+,"[^"]*"\]' | head -n 1 | sed -E 's/.*,"([^"]*)"\]/\1/'; }
    # name | PromQL | allowed growth: end <= start * factor + slack
    while IFS='|' read -r name query factor slack; do
      a="$(prom_at "avg_over_time(($query)[2m:15s])" "$early")"; z="$(prom_at "avg_over_time(($query)[2m:15s])" "$late")"
      if awk -v a="$a" -v z="$z" -v f="$factor" -v s="$slack" 'BEGIN { exit !(a != "" && z != "" && z <= a * f + s) }'; then
        pass "soak: $name flat (minutes 2-4: $(printf '%.1f' "$a"), last 2 min: $(printf '%.1f' "$z"))"
      else
        fail "soak: $name grew (minutes 2-4: '${a}', last 2 min: '${z}', allowed x$factor + $slack)"
      fi
    done << 'EOF'
api heap in use (MiB)|go_memstats_heap_inuse_bytes{job="api"} / 1048576|1.3|8
api goroutines|go_goroutines{job="api"}|1.3|20
worker heap in use (MiB)|go_memstats_heap_inuse_bytes{job="worker"} / 1048576|1.3|8
worker goroutines|go_goroutines{job="worker"}|1.3|20
api DB connections (open)|sum(seatlock_db_pool_connections{job="api",state="total"})|1.0|5
api Redis connections (open)|sum(seatlock_redis_pool_connections{job="api",state="total"})|1.0|5
PENDING outbox rows|sum(seatlock_outbox_rows{status="PENDING"}) or vector(0)|1.0|10
oldest PENDING payment (s)|max(seatlock_payments_pending_oldest_age_seconds) or vector(0)|1.0|30
EOF
    echo "  (soak ran $((end - start))s)"
  fi

  # k6 thresholds (unexpected_status, dropped_iterations, status_500 ...)
  if [ "$k6_rc" -eq 0 ]; then pass "k6 thresholds held"; else fail "k6 thresholds failed (exit $k6_rc): $(grep -E '✗' "$RUN.k6.log" | head -5 | tr -s ' ' | tr '\n' ' ')"; fi
  s500="$(result_get "$RUN.result.json" counters.status_500)"
  if [ "${s500:-0}" = "0" ]; then pass "no HTTP 500 responses"; else fail "$s500 HTTP 500 responses: a bug, investigate"; fi
  if [ "$STRICT" = "1" ]; then
    unx="$(result_get "$RUN.result.json" counters.unexpected_status)"
    if [ "${unx:-0}" = "0" ]; then pass "unexpected_status == 0"; else fail "unexpected_status = $unx"; fi
  fi

  echo "  --- docker stats during the run (k6 shares the CPUs with the system under test)"
  peaks "$RUN.stats.csv" | tee "$RUN.peaks.txt"
  if [ "$FAILURES" -eq 0 ]; then echo "RUN $i: PASS"; echo PASS > "$RUN.verdict"; else echo "RUN $i: FAIL ($FAILURES)"; echo FAIL > "$RUN.verdict"; fi
done

echo
if [ "$RUNS" -gt 1 ]; then
  go run ./loadtest/report -median "$OUT_DIR"/run*.result.json | tee "$OUT_DIR/median.txt"
fi
verdicts="$(cat "$OUT_DIR"/run*.verdict 2> /dev/null | sort | uniq -c | tr -s ' ' | tr '\n' ' ')"
echo "=== $SCENARIO:$verdicts— results in $OUT_DIR"
! grep -q FAIL "$OUT_DIR"/run*.verdict 2> /dev/null
