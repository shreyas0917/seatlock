#!/usr/bin/env bash
# expired-during-payment.sh - the hold runs out while the payment is in flight.
#
#   1. user holds a seat                                          -> 201
#      (the hold is shortened to 1 second with SQL, standing in for
#       "5 minutes have almost passed" without restarting the API)
#   2. the gateway is told to answer SLOWLY (3s); it takes the money,
#      but by then the hold has expired                           -> 410 HOLD_EXPIRED
#      body has "payment": "REFUNDED"; NO booking is created
#   3. payment REFUNDED, outbox REFUND row DONE, gateway shows 1 charge + 1 refund
#   4. demo/verify.sql                                            -> every check returns 0 rows
#
# Usage:  ./demo/expired-during-payment.sh   (needs: make up && make seed; takes ~3s)

set -uo pipefail
cd "$(dirname "$0")/.."
source demo/lib.sh

RUN_ID="$(date +%s)"
USER1="expire-$RUN_ID"
KEY="expire-key-$RUN_ID"
mock_reset

SEAT="$(available_seats 1)"
[ -n "$SEAT" ] || { echo "No AVAILABLE seat. Reset with: make reset-data"; exit 1; }

echo "Step 1: $USER1 holds seat $SEAT, and the hold is shortened to 1 second"
request POST /v1/holds "$USER1" "{\"show_id\": $SHOW_ID, \"seat_id\": $SEAT}"
expect_status 201 "hold"
HOLD="$(json_field hold_id)"
sql "UPDATE holds SET expires_at = NOW() + interval '1 second' WHERE id = $HOLD" > /dev/null

echo
echo "Step 2: confirm while the gateway is slow (3s > 1s hold) ..."
mock_queue slow
request POST /v1/bookings "$USER1" "{\"hold_id\": $HOLD}" "$KEY"
echo "  $BODY"
expect_status 410 "confirm after the hold expired"
echo "$BODY" | grep -q '"payment":"REFUNDED"' && pass "response says the money was REFUNDED" || fail "unexpected body: $BODY"

echo
echo "Step 3: database and gateway state"
expect_sql "SELECT COUNT(*) FROM bookings WHERE hold_id = $HOLD" 0 "bookings for the hold (none)"
expect_sql "SELECT status FROM payments WHERE hold_id = $HOLD" REFUNDED "payment status"
expect_sql "SELECT status FROM holds WHERE id = $HOLD" EXPIRED "hold status"
expect_sql "SELECT o.status FROM outbox o JOIN payments p ON (o.payload->>'payment_id')::bigint = p.id WHERE o.kind = 'REFUND' AND p.hold_id = $HOLD" DONE "refund outbox row"
expect_mock money_taken_charges 1 "one charge"
expect_mock total_refunds 1 "one refund"
mock_stats

run_verify
finish
