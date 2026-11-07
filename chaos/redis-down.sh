#!/usr/bin/env bash
# chaos/redis-down.sh — Redis is KILLED for 30 s in the middle of steady load.
#
# Expected (the fail-closed design):
#   - new holds -> 503 LOCK_UNAVAILABLE (nobody can grab a seat without the lock)
#   - confirms of holds made BEFORE the outage still succeed (they don't need Redis)
#   - the RedisLockUnavailable alert fires
#   - after Redis restarts, holds work again by themselves
#   - 0 invariant violations, 0 double charges
source "$(dirname "$0")/lib.sh"

start_load
wait_until_t 30

# Five users hold a seat on the rush show just before the outage.
declare -a HOLDS USERS
for i in 0 1 2 3 4; do
  USERS[$i]="chaos-redis-$i-$$"
  out="$(api_hold "$(rush_seat_id "$i")" "${USERS[$i]}")"
  HOLDS[$i]="$(echo "$out" | grep -oE '"hold_id":[0-9]+' | cut -d: -f2)"
  [ "${out%% *}" = "201" ] || fail "setup hold $i: $out"
done
pass "5 holds made before the outage"

docker compose kill redis > /dev/null 2>&1
DOWN_AT=$(date +%s)
event "Redis killed"
sleep 3

out="$(api_hold "$(rush_seat_id 50)" "chaos-redis-new-$$")"
if [ "${out%% *}" = "503" ] && echo "$out" | grep -q LOCK_UNAVAILABLE; then pass "new hold while Redis is down -> 503 LOCK_UNAVAILABLE (fail closed)"
else fail "new hold while Redis is down: expected 503 LOCK_UNAVAILABLE, got $out"; fi

ok=0
for i in 0 1 2 3 4; do
  out="$(api_confirm "${HOLDS[$i]}" "${USERS[$i]}" "chaos-redis-key-$i-$$")"
  [ "${out%% *}" = "201" ] && ok=$((ok + 1)) || echo "    confirm $i: $out"
done
if [ "$ok" = "5" ]; then pass "confirms of existing holds while Redis is down: 5/5 booked"
else fail "confirms of existing holds while Redis is down: only $ok/5 booked"; fi

left=$((DOWN_AT + 30 - $(date +%s))) # a 30 s outage, timed from the kill
[ "$left" -gt 0 ] && sleep "$left"
docker compose start redis > /dev/null 2>&1
UP_AT=$(date +%s)
event "Redis started"

# Recovery: the first new hold that succeeds again.
recovered=""
for n in $(seq 1 60); do
  out="$(api_hold "$(rush_seat_id $((50 + n)))" "chaos-redis-rec-$n-$$")"
  if [ "${out%% *}" = "201" ]; then recovered=$(($(date +%s) - UP_AT)); break; fi
  sleep 1
done
if [ -n "$recovered" ]; then pass "holds work again ${recovered}s after Redis restarted (outage: $((UP_AT - DOWN_AT))s)"; event "first hold OK again"
else fail "holds did not recover within 60s of Redis restarting"; fi

finish_load
unavail="$(result_get "$OUT_DIR/load.result.json" counters.lock_unavailable_503)"
if [ "${unavail:-0}" -gt 0 ]; then pass "k6 users saw the fail-closed answer: $unavail x 503 LOCK_UNAVAILABLE"
else fail "k6 users saw no 503 LOCK_UNAVAILABLE"; fi

# RedisLockUnavailable needs 1 min of refused holds (for: 1m): the outage is
# 30 s, but rate()[1m] stays > 0 for 60 s after the last refusal, so it fires.
fired=""
for _ in $(seq 1 24); do
  if alert_fired RedisLockUnavailable "$DOWN_AT"; then fired=1; break; fi
  sleep 5
done
if [ -n "$fired" ]; then pass "alert RedisLockUnavailable fired"; else fail "alert RedisLockUnavailable did not fire"; fi

echo
echo "--- correctness after the outage"
correctness_checks "$OUT_DIR/load" "" "" atleast
chaos_done
