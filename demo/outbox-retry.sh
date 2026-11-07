#!/usr/bin/env bash
# outbox-retry.sh - a refund fails, and the WORKER retries it with backoff.
#
#   1. user holds a seat; the hold is shortened to 1 second         -> 201
#   2. confirm while the gateway is slow (2s > 1s hold): money taken,
#      hold expired -> refund owed. The gateway is told to FAIL the next
#      2 refunds, so the inline refund fails                         -> 410, "payment":"REFUND_PENDING"
#   3. the outbox REFUND row is retried by the worker's drainer with
#      exponential backoff: attempt 1 (inline) fails, attempt 2 fails,
#      attempt 3 succeeds                                            -> REFUNDED, outbox DONE, 3 attempts
#   4. 1 charge, 1 refund, no booking; demo/verify.sql              -> every check returns 0 rows
#
# Usage:  make up-demo && ./demo/outbox-retry.sh      (about 15 seconds)

set -uo pipefail
cd "$(dirname "$0")/.."
source demo/lib.sh
require_demo_mode

RUN_ID="$(date +%s)"
USER1="refund-$RUN_ID"
mock_reset

SEAT="$(available_seats 1)"
[ -n "$SEAT" ] || { echo "No AVAILABLE seat. Reset with: make reset-data"; exit 1; }

echo "Step 1: $USER1 holds seat $SEAT; the hold is shortened to 1 second"
request POST /v1/holds "$USER1" "{\"show_id\": $SHOW_ID, \"seat_id\": $SEAT}"
expect_status 201 "hold"
HOLD="$(json_field hold_id)"
sql "UPDATE holds SET expires_at = NOW() + interval '1 second' WHERE id = $HOLD" > /dev/null

echo
echo "Step 2: slow charge (hold expires meanwhile), and the next 2 refunds will fail"
mock_queue slow
curl -fsS -X POST "$MOCK_URL/debug/next-behavior" -d '{"behavior":"refund_fail","count":2}' > /dev/null
request POST /v1/bookings "$USER1" "{\"hold_id\": $HOLD}" "refund-key-$RUN_ID"
echo "  $BODY"
expect_status 410 "confirm after the hold expired"
echo "$BODY" | grep -q '"payment":"REFUND_PENDING"' && pass "inline refund failed: REFUND_PENDING" || fail "unexpected body: $BODY"
OUTBOX="$(sql "SELECT o.id FROM outbox o JOIN payments p ON (o.payload->>'payment_id')::bigint = p.id WHERE o.kind = 'REFUND' AND p.hold_id = $HOLD" | tr -d '[:space:]')"
echo "  outbox row $OUTBOX: $(sql "SELECT 'attempts=' || attempts || ' next_attempt_in=' || round(EXTRACT(EPOCH FROM next_attempt_at - NOW())) || 's last_error=' || last_error FROM outbox WHERE id = $OUTBOX")"

echo
echo "Step 3: the worker retries with backoff ..."
LAST=""
for i in $(seq 1 40); do
  STATE="$(sql "SELECT status || ' attempts=' || attempts FROM outbox WHERE id = $OUTBOX")"
  if [ "$STATE" != "$LAST" ]; then echo "  t=${i}s outbox: $STATE"; LAST="$STATE"; fi
  echo "$STATE" | grep -q '^DONE' && break
  sleep 1
done
expect_sql "SELECT status FROM outbox WHERE id = $OUTBOX" DONE "outbox row"
expect_sql "SELECT attempts FROM outbox WHERE id = $OUTBOX" 3 "attempts (2 failures, then success)"
expect_sql "SELECT status FROM payments WHERE hold_id = $HOLD" REFUNDED "payment status"
expect_sql "SELECT COUNT(*) FROM bookings WHERE hold_id = $HOLD" 0 "bookings (none)"

echo
echo "Step 4: gateway"
expect_mock money_taken_charges 1 "one charge"
expect_mock total_refunds 1 "exactly one refund, despite the retries"
mock_stats
run_verify
finish
