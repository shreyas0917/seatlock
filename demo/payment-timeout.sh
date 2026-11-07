#!/usr/bin/env bash
# payment-timeout.sh - "money taken, answer lost".
#
#   1. user holds a seat                                         -> 201
#   2. the gateway is told to TAKE the money but never answer;
#      the API's 5s payment timeout fires                        -> 504 PAYMENT_PENDING
#      payment stays PENDING (unknown is NOT failed), no booking
#   3. the client retries with the SAME Idempotency-Key           -> 201 booking
#      the same charge_key is re-sent, the gateway replays its
#      stored "succeeded": still exactly 1 charge
#   4. demo/verify.sql                                           -> every check returns 0 rows
#
# Usage:  ./demo/payment-timeout.sh   (needs: make up && make seed; takes ~5s)

set -uo pipefail
cd "$(dirname "$0")/.."
source demo/lib.sh

RUN_ID="$(date +%s)"
USER1="timeout-$RUN_ID"
KEY="timeout-key-$RUN_ID"
mock_reset

SEAT="$(available_seats 1)"
[ -n "$SEAT" ] || { echo "No AVAILABLE seat. Reset with: make reset-data"; exit 1; }

echo "Step 1: $USER1 holds seat $SEAT"
request POST /v1/holds "$USER1" "{\"show_id\": $SHOW_ID, \"seat_id\": $SEAT}"
expect_status 201 "hold"
HOLD="$(json_field hold_id)"

echo
echo "Step 2: the gateway takes the money but the answer is lost (waiting for the 5s timeout ...)"
mock_queue timeout
request POST /v1/bookings "$USER1" "{\"hold_id\": $HOLD}" "$KEY"
echo "  $BODY"
expect_status 504 "confirm with a lost answer"
echo "$BODY" | grep -q PAYMENT_PENDING && pass "error code is PAYMENT_PENDING" || fail "unexpected body: $BODY"
expect_sql "SELECT status FROM payments WHERE hold_id = $HOLD" PENDING "payment status (unknown is not failed)"
expect_sql "SELECT COUNT(*) FROM bookings WHERE hold_id = $HOLD" 0 "bookings so far"
expect_mock money_taken_charges 1 "the gateway really took the money"

echo
echo "Step 3: retry with the SAME Idempotency-Key"
request POST /v1/bookings "$USER1" "{\"hold_id\": $HOLD}" "$KEY"
echo "  $BODY"
expect_status 201 "retry books the seat"
expect_sql "SELECT status FROM payments WHERE hold_id = $HOLD" SUCCEEDED "payment status"
expect_mock total_charges 1 "still exactly one charge"
mock_stats

run_verify
finish
