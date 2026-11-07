#!/usr/bin/env bash
# worker-reconcile.sh - the payment times out and the user NEVER retries.
#
#   1. user holds a seat                                         -> 201
#   2. confirm: the gateway takes the money but the answer is lost -> 504 PAYMENT_PENDING
#   3. the user gives up (no retry). The payment is PENDING; money WAS taken.
#   4. the worker's reconciler (after RECONCILE_AFTER = 15s) asks the gateway,
#      learns the charge succeeded, and runs the SAME Tx B as confirm
#                                                                -> payment SUCCEEDED, booking created
#   5. exactly 1 charge, 1 booking; demo/verify.sql              -> every check returns 0 rows
#
# Usage:  make up-demo && ./demo/worker-reconcile.sh      (about 25 seconds)

set -uo pipefail
cd "$(dirname "$0")/.."
source demo/lib.sh
require_demo_mode

RUN_ID="$(date +%s)"
USER1="reconcile-$RUN_ID"
mock_reset

SEAT="$(available_seats 1)"
[ -n "$SEAT" ] || { echo "No AVAILABLE seat. Reset with: make reset-data"; exit 1; }

echo "Step 1: $USER1 holds seat $SEAT"
request POST /v1/holds "$USER1" "{\"show_id\": $SHOW_ID, \"seat_id\": $SEAT}"
expect_status 201 "hold"
HOLD="$(json_field hold_id)"

echo
echo "Step 2: confirm; the gateway takes the money but never answers (3s timeout) ..."
mock_queue timeout
request POST /v1/bookings "$USER1" "{\"hold_id\": $HOLD}" "reconcile-key-$RUN_ID"
echo "  $BODY"
expect_status 504 "confirm"
expect_sql "SELECT status FROM payments WHERE hold_id = $HOLD" PENDING "payment status"
expect_mock money_taken_charges 1 "the gateway took the money"

echo
echo "Step 3+4: the user never retries; waiting for the worker's reconciler ..."
wait_for "SELECT status FROM payments WHERE hold_id = $HOLD" SUCCEEDED 60 "payment status"
expect_sql "SELECT COUNT(*) FROM bookings WHERE hold_id = $HOLD" 1 "bookings for the hold"
expect_sql "SELECT reconcile_attempts FROM payments WHERE hold_id = $HOLD" 1 "reconcile attempts"
SEATMAP="$(curl -fsS "$API_URL/v1/shows/$SHOW_ID/seats" | grep -oE "\"id\":$SEAT,[^}]*")"
echo "$SEATMAP" | grep -q '"status":"BOOKED"' && pass "seat map: seat $SEAT is BOOKED" || fail "seat map: $SEATMAP"

echo
echo "Step 5: one charge, one booking"
expect_mock total_charges 1 "exactly one charge"
mock_stats
run_verify
finish
