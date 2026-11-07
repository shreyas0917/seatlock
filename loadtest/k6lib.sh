#!/usr/bin/env bash
# loadtest/k6lib.sh — shared by loadtest/run.sh and the chaos/ scripts.
# Source it from the repo root, AFTER demo/lib.sh:
#   source demo/lib.sh; source loadtest/k6lib.sh
#
#   k6 SCRIPT SUMMARY [args]   run loadtest/SCRIPT in Docker inside the Compose network
#   reset_all                  free every seat (Postgres + Redis) and reset the gateway
#   sample_stats FILE          `docker stats` snapshots until killed (run it with &)
#   peaks FILE                 peak CPU / memory per container from those snapshots
#   correctness_checks RUN [SHOW BOOKINGS]
#                              time to consistency, bookings, gateway charges,
#                              verify.sql, fresh Prometheus invariants

K6_IMAGE="grafana/k6:0.54.0"
BASE_URL="${BASE_URL:-http://api:8080}"
NETWORK="${NETWORK:-seatlock_default}"
K6_ENV="${K6_ENV:-}"
# Docker on Windows needs a Windows path for bind mounts (Git Bash: pwd -W).
HOST_DIR="$(pwd -W 2> /dev/null || pwd)"
export MSYS_NO_PATHCONV=1

# k6 SCRIPT SUMMARY_FILE|"" [extra k6 args]. OUT_DIR must be set (relative).
# K6_NAME names the container (default seatlock-k6), so docker stats can find it.
k6() {
  local script="$1" summary="$2"; shift 2
  local name="${K6_NAME:-seatlock-k6}"
  local args=(run -q -e "BASE_URL=$BASE_URL" -e "RUN_ID=$(date +%s%N | cut -c1-13)")
  local kv
  for kv in $K6_ENV; do args+=(-e "$kv"); done
  [ -n "$summary" ] && args+=(--summary-export "/out/$(basename "$summary")")
  # The k6 image runs as an unprivileged user; on Linux (CI) it may not
  # write into a directory owned by the runner's user otherwise.
  chmod a+rwx "$OUT_DIR"
  docker rm -f "$name" > /dev/null 2>&1
  docker run --rm --name "$name" --network "$NETWORK" \
    -v "$HOST_DIR/loadtest:/scripts:ro" -v "$HOST_DIR/$OUT_DIR:/out" \
    "$K6_IMAGE" "${args[@]}" "$@" "/scripts/$script"
}

reset_all() {
  make -s reset-data > /dev/null 2>&1 || { echo "reset-data failed (is the stack up?)"; exit 1; }
  mock_reset
}

sample_stats() {
  while true; do
    docker stats --no-stream --format '{{.Name}},{{.CPUPerc}},{{.MemUsage}}' 2> /dev/null \
      | grep -E 'seatlock' | sed "s/^/$(date +%H:%M:%S),/" >> "$1"
  done
}

peaks() {
  awk -F, '{
      name=$2; cpu=$3; sub(/%/,"",cpu); split($4, m, " / "); mem=m[1]; v=mem+0
      if (mem ~ /GiB/) v*=1024; else if (mem ~ /KiB/) v/=1024; else if (mem ~ /[0-9]B$/) v/=1048576
      if (cpu+0 > c[name]) c[name]=cpu+0; if (v > mm[name]) mm[name]=v; n[name]++
    } END { for (k in c) printf "  %-28s peak CPU %6.1f%%  peak mem %7.1f MiB  (%d samples)\n", k, c[k], mm[k], n[k] }' "$1" | sort
}

# result_get RESULT_JSON PATH  (e.g. counters.bookings_ok)
result_get() { go run ./loadtest/report -get "$2" "$1"; }

# wait_consistent LIMIT_S: polls until 0 PENDING payments and 0 PENDING
# outbox rows. Sets CONSISTENT_AFTER (seconds) or leaves it empty on timeout.
wait_consistent() {
  local limit="$1" t0 pend
  t0=$(date +%s); CONSISTENT_AFTER=""
  while [ $(($(date +%s) - t0)) -lt "$limit" ]; do
    pend="$(sql "SELECT (SELECT COUNT(*) FROM payments WHERE status = 'PENDING') + (SELECT COUNT(*) FROM outbox WHERE status = 'PENDING')" | tr -d '[:space:]')"
    if [ "$pend" = "0" ]; then CONSISTENT_AFTER=$(($(date +%s) - t0)); return 0; fi
    sleep 2
  done
  return 1
}

# correctness_checks RUN [EXPECT_SHOW EXPECT_BOOKINGS] [K6_BOOKINGS_MODE]
#   RUN: path prefix of the run (RUN.result.json, RUN.verify.txt)
#   K6_BOOKINGS_MODE: "equal" (default): Postgres bookings == bookings k6 saw;
#                     "atleast": >= (chaos: a booking can commit while its
#                     answer is lost, e.g. the replica was killed)
correctness_checks() {
  local run="$1" show="${2:-}" want="${3:-}" mode="${4:-equal}"
  local t0 db_bookings k6_bookings stats taken refunds max_per_key max_refund vout checks zero last fresh

  # 1. time to consistency
  t0=$(date +%s)
  if wait_consistent "${CONSISTENCY_TIMEOUT:-300}"; then
    pass "time to consistency: ${CONSISTENT_AFTER}s (0 PENDING payments, 0 PENDING outbox rows)"
  else
    fail "still PENDING payments/outbox rows after ${CONSISTENCY_TIMEOUT:-300}s"
  fi

  # 2. bookings
  db_bookings="$(sql 'SELECT COUNT(*) FROM bookings' | tr -d '[:space:]')"
  k6_bookings="$(result_get "$run.result.json" counters.bookings_ok)"
  if [ "$mode" = "atleast" ]; then
    if [ "$db_bookings" -ge "${k6_bookings:-0}" ]; then pass "bookings in Postgres ($db_bookings) >= bookings k6 saw ($k6_bookings)"
    else fail "bookings in Postgres $db_bookings < bookings k6 saw $k6_bookings"; fi
  else
    # A confirm answered 504 PAYMENT_PENDING has an UNKNOWN outcome for the
    # client; the payment is resolved later (Tx B retry or the reconciler)
    # and may well become a booking. So Postgres may hold up to that many
    # bookings more than k6 saw, never fewer, never more than that.
    c504="$(result_get "$run.result.json" counters.confirm_504)"
    extra=$((db_bookings - ${k6_bookings:-0}))
    if [ "$extra" -eq 0 ]; then pass "bookings in Postgres ($db_bookings) == bookings k6 saw ($k6_bookings)"
    elif [ "$extra" -gt 0 ] && [ "$extra" -le "${c504:-0}" ]; then
      pass "bookings in Postgres ($db_bookings) == bookings k6 saw ($k6_bookings) + $extra of the ${c504} confirms answered 504 PAYMENT_PENDING, resolved later"
    else fail "bookings in Postgres $db_bookings vs bookings k6 saw $k6_bookings (confirms answered 504: ${c504:-0})"; fi
  fi
  if [ -n "$show" ]; then
    expect_sql "SELECT COUNT(*) FROM bookings b JOIN shows s ON s.id = b.show_id WHERE s.title = '$show'" "$want" "bookings on '$show'"
  fi

  # 3. gateway: money taken == bookings + refunds; never 2 charges or 2 refunds per key
  stats="$(curl -fsS "$MOCK_URL/debug/stats")"
  taken="$(echo "$stats" | grep -oE '"money_taken_charges":[0-9]+' | cut -d: -f2)"
  refunds="$(echo "$stats" | grep -oE '"total_refunds":[0-9]+' | cut -d: -f2)"
  max_per_key="$(echo "$stats" | grep -oE '"charges_created":\{[^}]*\}' | grep -oE ':[0-9]+' | tr -d : | sort -n | tail -1)"
  max_refund="$(echo "$stats" | grep -oE '"refunds_created":\{[^}]*\}' | grep -oE ':[0-9]+' | tr -d : | sort -n | tail -1)"
  if [ "$taken" = "$((db_bookings + refunds))" ]; then pass "gateway: charges that took money ($taken) == bookings ($db_bookings) + refunds ($refunds)"
  else fail "gateway: charges that took money $taken != bookings $db_bookings + refunds $refunds"; fi
  if [ "${max_per_key:-0}" -le 1 ] && [ "${max_refund:-0}" -le 1 ]; then pass "gateway: at most 1 charge and 1 refund per idempotency key (0 double charges)"
  else fail "double charge/refund: max charges per key ${max_per_key}, max refunds per key ${max_refund}"; fi

  # 4. verify.sql
  vout="$(docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U "$PG_USER" -d "$PG_DB" < demo/verify.sql)"
  echo "$vout" > "$run.verify.txt"
  checks="$(echo "$vout" | grep -c '^CHECK')"; zero="$(echo "$vout" | grep -c '^(0 rows)')"
  if [ "$checks" -gt 0 ] && [ "$checks" = "$zero" ]; then pass "verify.sql: all $checks checks returned 0 rows"
  else fail "verify.sql: only $zero of $checks checks returned 0 rows (see $run.verify.txt)"; fi

  # 5. Prometheus: an invariant run that finished after these checks began
  # (skipped with NO_PROM=1: the CI smoke test runs without Prometheus).
  if [ -n "${NO_PROM:-}" ]; then
    echo "  SKIP: Prometheus invariant gauges (NO_PROM=1; verify.sql above runs the same checks)"
    return
  fi
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
}
