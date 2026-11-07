#!/usr/bin/env bash
# redis-down.sh - proves the FAIL-CLOSED policy against the real running stack.
#
#   1. user1 holds seat X, user3 holds seat Z            -> 201, 201
#   2. Redis is STOPPED (docker compose stop redis)
#   3. user2 tries to hold seat Y                        -> 503 LOCK_UNAVAILABLE (fail closed)
#      /readyz                                           -> 200, redis "down" (API stays in service)
#   4. user1 confirms the hold from step 1               -> 201 (existing reservation completes)
#   5. a fake hold id / another user's hold              -> 404 HOLD_NOT_FOUND
#      user3 cancels their hold                          -> 200 (works without Redis)
#   6. Redis is STARTED again and healthy -> user2's hold on Y works -> 201
#   7. demo/verify.sql                                   -> every check returns 0 rows
#
# Every step prints PASS or FAIL; the script exits non-zero if anything failed.
# Redis is always restarted at the end, even if the script fails halfway.
#
# Usage:  ./demo/redis-down.sh      (API must be running: make up && make seed)

set -uo pipefail
cd "$(dirname "$0")/.."
source demo/lib.sh

RUN_ID="$(date +%s)"
USER1="rd-$RUN_ID-user1"
USER2="rd-$RUN_ID-user2"
USER3="rd-$RUN_ID-user3"

# If anything goes wrong, never leave Redis stopped.
trap 'docker compose start redis >/dev/null 2>&1' EXIT

read -r SEAT_X SEAT_Y SEAT_Z <<< "$(available_seats 3 | tr '\n' ' ')"
if [ -z "${SEAT_Z:-}" ]; then
  echo "Need 3 AVAILABLE seats in show $SHOW_ID."
  echo "Reset with: docker compose exec -T postgres psql -U seatlock -d seatlock -c 'TRUNCATE bookings, holds'"
  exit 1
fi
echo "Using show $SHOW_ID, seats X=$SEAT_X Y=$SEAT_Y Z=$SEAT_Z"

echo
echo "Step 1: holds while Redis is up"
request POST /v1/holds "$USER1" "{\"show_id\": $SHOW_ID, \"seat_id\": $SEAT_X}"
expect_status 201 "user1 holds seat X"
HOLD1="$(json_field hold_id)"
request POST /v1/holds "$USER3" "{\"show_id\": $SHOW_ID, \"seat_id\": $SEAT_Z}"
expect_status 201 "user3 holds seat Z"
HOLD3="$(json_field hold_id)"

echo
echo "Step 2: stopping Redis"
docker compose stop redis >/dev/null 2>&1 && pass "redis stopped" || fail "could not stop redis"

echo
echo "Step 3: a NEW hold must fail closed"
request POST /v1/holds "$USER2" "{\"show_id\": $SHOW_ID, \"seat_id\": $SEAT_Y}"
expect_status 503 "user2 holds seat Y with Redis down"
if echo "$BODY" | grep -q LOCK_UNAVAILABLE; then pass "error code is LOCK_UNAVAILABLE"; else fail "unexpected body: $BODY"; fi
request GET /readyz "" ""
expect_status 200 "readyz with Redis down (API stays in service)"
if echo "$BODY" | grep -q '"redis":"down"'; then pass "readyz reports $BODY"; else fail "readyz body: $BODY"; fi

echo
echo "Step 4: an EXISTING hold still confirms (PostgreSQL only)"
request POST /v1/bookings "$USER1" "{\"hold_id\": $HOLD1}"
expect_status 201 "user1 confirms hold $HOLD1 with Redis down"

echo
echo "Step 5: invalid confirms are still rejected"
request POST /v1/bookings "$USER1" '{"hold_id": 999999999}'
expect_status 404 "confirm a fake hold id"
request POST /v1/bookings "$USER2" "{\"hold_id\": $HOLD3}"
expect_status 404 "user2 confirms user3's hold"
request DELETE "/v1/holds/$HOLD3" "$USER3" ""
expect_status 200 "user3 cancels their hold with Redis down"

echo
echo "Step 6: starting Redis and waiting until it is healthy"
if docker compose up -d --wait redis >/dev/null 2>&1; then pass "redis is healthy"; else fail "redis did not become healthy"; fi
# The API reconnects to Redis on demand; allow a few seconds for that.
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  request POST /v1/holds "$USER2" "{\"show_id\": $SHOW_ID, \"seat_id\": $SEAT_Y}"
  [ "$STATUS" = "201" ] && break
  sleep 1
done
expect_status 201 "user2 holds seat Y after recovery (attempt $attempt)"

echo
echo "Step 7: invariants"
run_verify
finish
