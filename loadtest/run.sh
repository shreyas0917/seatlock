#!/usr/bin/env bash
# loadtest/run.sh — runs one k6 scenario the SAME way every time, then checks
# that SeatLock stayed correct.
#
#   loadtest/run.sh <scenario> [runs]       scenario = rush | capacity | spike | soak | smoke
#
#   RUNS default 1. Each run: reset data + gateway -> 15 s warm-up -> reset
#   again -> measured k6 run (k6 in Docker, INSIDE the Compose network, so the
#   Windows/macOS host port proxy is not what gets measured) -> checks:
#     1. time to consistency: 0 PENDING payments and 0 PENDING outbox rows
#     2. bookings in Postgres == bookings k6 saw (and == seats, for rush/smoke)
#     3. gateway: charges that took money == bookings + refunds, <= 1 per key
#     4. demo/verify.sql: every check 0 rows
#     5. Prometheus: a FRESH invariant run reports all 11 checks, max 0
#     6. k6 thresholds held (unexpected_status == 0 where required)
#   It also samples `docker stats` (k6 itself, api, postgres, redis...) for
#   the whole run: k6 shares the CPUs with the system under test.
#
# Env: K6_ENV="CAP_RATE=400 X=1" (extra k6 -e vars), BASE_URL (default
# http://api:8080), OUT_DIR, SKIP_WARMUP=1, NETWORK (default seatlock_default).
# Needs the stack up (make up-bench) and the load data (make seed-load).
set -u
cd "$(dirname "$0")/.." || exit 1
# shellcheck source=../demo/lib.sh
source demo/lib.sh

K6_IMAGE="grafana/k6:0.54.0"
SCENARIO="${1:?usage: loadtest/run.sh <rush|capacity|spike|soak|smoke> [runs]}"
RUNS="${2:-${RUNS:-1}}"
BASE_URL="${BASE_URL:-http://api:8080}"
NETWORK="${NETWORK:-seatlock_default}"
K6_ENV="${K6_ENV:-}"

# Active duration of each scenario (for RPS) and the exact booking count, if any.
EXPECT_SHOW="" EXPECT_BOOKINGS="" STRICT=1
case "$SCENARIO" in
  rush) DURATION=10s; EXPECT_SHOW="Load: Rush"; EXPECT_BOOKINGS=100 ;;
  smoke) DURATION=10s; EXPECT_SHOW="Load: Smoke"; EXPECT_BOOKINGS=20 ;;
  capacity) DURATION="$(echo "$K6_ENV" | grep -oE 'CAP_DURATION=[^ ]+' | cut -d= -f2)"; DURATION="${DURATION:-60s}"; STRICT=0 ;;
  spike) DURATION=150s ;;
  soak) DURATION="$(echo "$K6_ENV" | grep -oE 'SOAK_DURATION=[^ ]+' | cut -d= -f2)"; DURATION="${DURATION:-20m}" ;;
  *) echo "unknown scenario $SCENARIO"; exit 2 ;;
esac

OUT_DIR="${OUT_DIR:-tmp/loadtest/$SCENARIO-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$OUT_DIR"
# Docker on Windows needs a Windows path for the bind mount (Git Bash: pwd -W).
HOST_DIR="$(pwd -W 2>/dev/null || pwd)"
export MSYS_NO_PATHCONV=1

k6() { # k6 <script> <summary-file|""> [extra args...]
  local script="$1" summary="$2"; shift 2
  local args=(run -q -e "BASE_URL=$BASE_URL" -e "RUN_ID=$(date +%s%N | cut -c1-13)")
  for kv in $K6_ENV; do args+=(-e "$kv"); done
  [ -n "$summary" ] && args+=(--summary-export "/out/$(basename "$summary")")
  docker rm -f seatlock-k6 > /dev/null 2>&1
  docker run --rm --name seatlock-k6 --network "$NETWORK" \
    -v "$HOST_DIR/loadtest:/scripts:ro" -v "$HOST_DIR/$OUT_DIR:/out" \
    "$K6_IMAGE" "${args[@]}" "$@" "/scripts/$script"
}

reset_all() {
  make -s reset-data > /dev/null 2>&1 || { echo "reset-data failed (is the stack up?)"; exit 1; }
  mock_reset
}

# sample_stats FILE: one `docker stats` snapshot every ~2 s until killed.
sample_stats() {
  while true; do
    docker stats --no-stream --format '{{.Name}},{{.CPUPerc}},{{.MemUsage}}' 2>/dev/null \
      | grep -E 'seatlock' | sed "s/^/$(date +%H:%M:%S),/" >> "$1"
  done
}

# peaks FILE: peak CPU% and memory per container.
peaks() {
  awk -F, '{
      name=$2; cpu=$3; sub(/%/,"",cpu); split($4, m, " / "); mem=m[1]; v=mem+0
      if (mem ~ /GiB/) v*=1024; else if (mem ~ /KiB/) v/=1024; else if (mem ~ /[^i]B$/ && mem !~ /MiB/) v/=1048576
      if (cpu+0 > c[name]) c[name]=cpu+0; if (v > mm[name]) mm[name]=v; n[name]++
    } END { for (k in c) printf "  %-28s peak CPU %6.1f%%  peak mem %7.1f MiB  (%d samples)\n", k, c[k], mm[k], n[k] }' "$1" | sort
}

result_value() { grep -oE "\"$2\": [0-9.]+" "$1" | head -1 | awk '{print $2}'; }

{
  echo "=== loadtest/run.sh $SCENARIO, $RUNS run(s), K6_ENV='$K6_ENV', $(date)"
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
    fail "k6 wrote no summary"; tail -20 "$RUN.k6.log"; continue
  fi
  go run ./loadtest/report -duration "$DURATION" "$RUN.json" | sed 's/^/  /'

  echo "  --- checks"
  # 1. time to consistency
  t0=$(date +%s) consistent=""
  while [ $(( $(date +%s) - t0 )) -lt 300 ]; do
    pend="$(sql "SELECT (SELECT COUNT(*) FROM payments WHERE status = 'PENDING') + (SELECT COUNT(*) FROM outbox WHERE status = 'PENDING')" | tr -d '[:space:]')"
    if [ "$pend" = "0" ]; then consistent=$(( $(date +%s) - t0 )); break; fi
    sleep 2
  done
  if [ -n "$consistent" ]; then pass "time to consistency: ${consistent}s (0 PENDING payments, 0 PENDING outbox rows)"
  else fail "still $pend PENDING payments/outbox rows after 300s"; fi

  # 2. bookings
  db_bookings="$(sql 'SELECT COUNT(*) FROM bookings' | tr -d '[:space:]')"
  k6_bookings="$(result_value "$RUN.result.json" bookings_ok)"
  if [ "$db_bookings" = "$k6_bookings" ]; then pass "bookings in Postgres ($db_bookings) == bookings k6 saw ($k6_bookings)"
  else fail "bookings in Postgres $db_bookings != bookings k6 saw $k6_bookings"; fi
  if [ -n "$EXPECT_SHOW" ]; then
    expect_sql "SELECT COUNT(*) FROM bookings b JOIN shows s ON s.id = b.show_id WHERE s.title = '$EXPECT_SHOW'" "$EXPECT_BOOKINGS" "bookings on '$EXPECT_SHOW'"
  fi

  # 3. gateway
  stats="$(curl -fsS "$MOCK_URL/debug/stats")"
  taken="$(echo "$stats" | grep -oE '"money_taken_charges":[0-9]+' | cut -d: -f2)"
  refunds="$(echo "$stats" | grep -oE '"total_refunds":[0-9]+' | cut -d: -f2)"
  max_per_key="$(echo "$stats" | grep -oE '"charges_created":\{[^}]*\}' | grep -oE ':[0-9]+' | tr -d : | sort -n | tail -1)"
  if [ "$taken" = "$((db_bookings + refunds))" ]; then pass "gateway: charges that took money ($taken) == bookings ($db_bookings) + refunds ($refunds)"
  else fail "gateway: charges that took money $taken != bookings $db_bookings + refunds $refunds"; fi
  if [ "${max_per_key:-0}" -le 1 ]; then pass "gateway: at most 1 charge per idempotency key"; else fail "a key was charged $max_per_key times"; fi

  # 4. verify.sql
  vout="$(docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U "$PG_USER" -d "$PG_DB" < demo/verify.sql)"
  echo "$vout" > "$RUN.verify.txt"
  checks="$(echo "$vout" | grep -c '^CHECK')"; zero="$(echo "$vout" | grep -c '^(0 rows)')"
  if [ "$checks" -gt 0 ] && [ "$checks" = "$zero" ]; then pass "verify.sql: all $checks checks returned 0 rows"
  else fail "verify.sql: only $zero of $checks checks returned 0 rows (see $RUN.verify.txt)"; fi

  # 5. Prometheus: wait for an invariant run that STARTED after the checks above began.
  fresh=""
  for _ in $(seq 1 30); do
    last="$(prom_value 'min(seatlock_invariant_last_success_timestamp_seconds)')"
    if [ -n "$last" ] && [ "${last%%.*}" -gt "$t0" ]; then fresh=1; break; fi
    sleep 2
  done
  if [ -n "$fresh" ]; then
    expect_prom 'count(seatlock_invariant_violations)' -eq 11 "Prometheus: invariant checks reporting"
    expect_prom 'max(seatlock_invariant_violations)' -eq 0 "Prometheus: largest invariant violation count"
  else
    fail "Prometheus: no fresh invariant run within 60s"
  fi

  # 6. k6 thresholds (unexpected_status, dropped_iterations, ...) and 500s
  if [ "$k6_rc" -eq 0 ]; then pass "k6 thresholds held"; else fail "k6 thresholds failed (exit $k6_rc): $(grep -E '✗|thresholds' "$RUN.k6.log" | head -5 | tr '\n' ' ')"; fi
  s500="$(result_value "$RUN.result.json" status_500)"
  if [ "${s500:-0}" = "0" ]; then pass "no HTTP 500 responses"; else fail "$s500 HTTP 500 responses: a bug, investigate"; fi
  if [ "$STRICT" = "1" ]; then
    unx="$(result_value "$RUN.result.json" unexpected_status)"
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
verdicts="$(cat "$OUT_DIR"/run*.verdict 2>/dev/null | sort | uniq -c | tr '\n' ' ')"
echo "=== $SCENARIO: $verdicts— results in $OUT_DIR"
! grep -q FAIL "$OUT_DIR"/run*.verdict 2>/dev/null
