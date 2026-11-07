#!/usr/bin/env bash
# abandon.sh - a user holds a seat and walks away. The WORKER cleans up.
#
#   1. user1 holds seat X                                   -> 201 (hold 20s, Redis lock 30s)
#   2. nobody confirms; the hold expires (database clock)
#   3. the worker's hold expirer marks it EXPIRED and writes an outbox
#      RELEASE_LOCK row in the same transaction; the outbox drainer deletes
#      the Redis lock                                        -> hold EXPIRED, outbox DONE, key gone
#   4. user2 holds seat X BEFORE the Redis TTL (30s) would have freed it -> 201
#   5. demo/verify.sql                                       -> every check returns 0 rows
#
# Usage:  make up-demo && ./demo/abandon.sh      (about 25 seconds)

set -uo pipefail
cd "$(dirname "$0")/.."
source demo/lib.sh
require_demo_mode

RUN_ID="$(date +%s)"
USER1="abandon-$RUN_ID-user1"
USER2="abandon-$RUN_ID-user2"
mock_reset

SEAT="$(available_seats 1)"
[ -n "$SEAT" ] || { echo "No AVAILABLE seat. Reset with: make reset-data"; exit 1; }
KEY="seat:$SHOW_ID:$SEAT"

echo "Step 1: $USER1 holds seat $SEAT"
START=$(date +%s)
request POST /v1/holds "$USER1" "{\"show_id\": $SHOW_ID, \"seat_id\": $SEAT}"
expect_status 201 "hold"
HOLD="$(json_field hold_id)"
[ "$(redis_has "$KEY")" = "1" ] && pass "Redis lock $KEY exists" || fail "Redis lock missing"

echo
echo "Step 2+3: $USER1 walks away; waiting for the worker (hold expires after 20s) ..."
wait_for "SELECT status FROM holds WHERE id = $HOLD" EXPIRED 45 "hold status"
wait_for "SELECT status FROM outbox WHERE kind = 'RELEASE_LOCK' AND (payload->>'hold_id')::bigint = $HOLD" DONE 15 "RELEASE_LOCK outbox row"
[ "$(redis_has "$KEY")" = "0" ] && pass "the Redis lock was released by the worker" || fail "the Redis lock is still there"

echo
echo "Step 4: $USER2 holds the same seat"
request POST /v1/holds "$USER2" "{\"show_id\": $SHOW_ID, \"seat_id\": $SEAT}"
ELAPSED=$(( $(date +%s) - START ))
expect_status 201 "$USER2 holds seat $SEAT, ${ELAPSED}s after the first hold"
if [ "$ELAPSED" -lt 30 ]; then
  pass "freed by the worker, before the 30s Redis TTL would have done it"
else
  echo "  (note: ${ELAPSED}s >= 30s, so the TTL could also explain it this time)"
fi
request DELETE "/v1/holds/$(json_field hold_id)" "$USER2" ""   # tidy up

mock_stats
run_verify
finish
