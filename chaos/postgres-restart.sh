#!/usr/bin/env bash
# chaos/postgres-restart.sh — Postgres is restarted (every connection is cut)
# in the middle of steady load.
#
# Expected:
#   - while it is down, the API answers 503 (SERVICE_BUSY), NEVER 500 and
#     never a wrong answer
#   - /readyz reports it (503 while Postgres is down, 200 again after)
#   - the API recovers BY ITSELF (the pool reconnects), no restart needed
#   - 0 double charges, verify.sql 0 rows, invariants 0
source "$(dirname "$0")/lib.sh"

start_load
wait_until_t 30
docker compose restart postgres > "$OUT_DIR/restart.log" 2>&1 &
RESTART_PID=$!
RESTART_AT=$(date +%s)
event "Postgres restart"

# Poll /readyz twice a second: when did it see Postgres down, and up again?
saw_down="" back_up=""
while [ $(($(date +%s) - RESTART_AT)) -lt 90 ]; do
  code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "$API_URL/readyz")"
  if [ "$code" = "503" ] && [ -z "$saw_down" ]; then saw_down=$(($(date +%s) - RESTART_AT)); event "/readyz 503 (postgres down)"; fi
  if [ "$code" = "200" ] && [ -n "$saw_down" ]; then back_up=$(($(date +%s) - RESTART_AT)); event "/readyz 200 again"; break; fi
  sleep 0.5
done
wait "$RESTART_PID"
[ -n "$saw_down" ] && pass "/readyz reported Postgres down (503) ${saw_down}s after the restart began" || fail "/readyz never reported Postgres down"
[ -n "$back_up" ] && pass "/readyz is 200 again ${back_up}s after the restart began" || fail "/readyz did not recover within 90s"

# Recovery of real traffic: the first new hold that succeeds.
recovered=""
for n in $(seq 1 60); do
  out="$(api_hold "$(rush_seat_id "$n")" "chaos-pg-$n-$$")"
  if [ "${out%% *}" = "201" ]; then recovered=$(($(date +%s) - RESTART_AT)); break; fi
  sleep 1
done
[ -n "$recovered" ] && { pass "holds succeed again ${recovered}s after the restart began, without restarting the API"; event "first hold OK again"; } \
  || fail "holds did not recover within 60s"

finish_load
busy="$(result_get "$OUT_DIR/load.result.json" counters.shed_503)"
echo "  users saw ${busy:-0} x 503 SERVICE_BUSY while Postgres was down"

echo
echo "--- correctness after the restart"
correctness_checks "$OUT_DIR/load" "" "" atleast
chaos_done
