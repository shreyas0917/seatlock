#!/usr/bin/env bash
# duplicate-request.sh - the "user double-clicks Pay" demo.
#
#   1. user holds a seat                                     -> 201
#   2. the SAME confirm (same Idempotency-Key) is sent 5x    -> 201, then 4 replays
#      (header Idempotent-Replayed: true, identical body)
#   3. exactly 1 booking and exactly 1 charge at the gateway
#   4. demo/verify.sql                                       -> every check returns 0 rows
#
# Usage:  ./demo/duplicate-request.sh   (needs: make up && make seed)

set -uo pipefail
cd "$(dirname "$0")/.."
source demo/lib.sh

RUN_ID="$(date +%s)"
USER1="dup-$RUN_ID"
KEY="dup-key-$RUN_ID"
mock_reset

SEAT="$(available_seats 1)"
[ -n "$SEAT" ] || { echo "No AVAILABLE seat. Reset with: make reset-data"; exit 1; }

echo "Step 1: $USER1 holds seat $SEAT"
request POST /v1/holds "$USER1" "{\"show_id\": $SHOW_ID, \"seat_id\": $SEAT}"
expect_status 201 "hold"
HOLD="$(json_field hold_id)"

echo
echo "Step 2: the same confirm, Idempotency-Key: $KEY, sent 5 times"
FIRST_BODY=""
for i in 1 2 3 4 5; do
  request POST /v1/bookings "$USER1" "{\"hold_id\": $HOLD}" "$KEY"
  echo "  send $i -> $STATUS replayed=$REPLAYED"
  if [ "$i" -eq 1 ]; then
    expect_status 201 "first send books"
    FIRST_BODY="$BODY"
  else
    [ "$STATUS" = "201" ] && [ "$REPLAYED" = "true" ] && [ "$BODY" = "$FIRST_BODY" ] \
      && pass "send $i is an identical replay" \
      || fail "send $i: status $STATUS replayed=$REPLAYED body=$BODY"
  fi
done
echo "  body: $FIRST_BODY"

echo
echo "Step 3: count bookings and charges"
expect_sql "SELECT COUNT(*) FROM bookings WHERE hold_id = $HOLD" 1 "bookings for the hold"
expect_mock total_charges 1 "exactly one charge at the gateway"
mock_stats

run_verify
finish
