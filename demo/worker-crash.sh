#!/usr/bin/env bash
# worker-crash.sh - the worker is KILLED in the middle of a backlog. Nothing is lost.
#
#   1. the worker is stopped; a backlog of 12 abandoned payments is created:
#      each charge SUCCEEDED at the gateway (money taken) but the answer was
#      lost, and nobody retried
#   2. the worker starts and begins working (batches of 3 every ~3s)
#   3. after the first batches it is KILLED (docker compose kill: SIGKILL, no
#      cleanup at all), possibly in the middle of a row
#   4. the worker starts again: claims of the dead worker expire (lease) and
#      every remaining row is processed
#   5. all 12 payments SUCCEEDED, 12 bookings (one per hold), 12 charges at
#      the gateway, no double booking; demo/verify.sql -> every check returns 0 rows
#
# Usage:  make up-demo && ./demo/worker-crash.sh      (about 40 seconds)

set -uo pipefail
cd "$(dirname "$0")/.."
source demo/lib.sh
require_demo_mode

N=12
RUN_ID="$(date +%s)"
COMPOSE="docker compose -f docker-compose.yml -f docker-compose.demo.yml"
mock_reset
trap '$COMPOSE up -d worker > /dev/null 2>&1' EXIT   # never leave the worker down

SEATS="$(available_seats $N)"
[ "$(echo "$SEATS" | wc -w)" -eq "$N" ] || { echo "Need $N AVAILABLE seats. Reset with: make reset-data"; exit 1; }

echo "Step 1: stop the worker and build a backlog of $N abandoned, successfully-charged payments"
$COMPOSE stop worker > /dev/null 2>&1 && pass "worker stopped"
HOLDS=""
for SEAT in $SEATS; do
  request POST /v1/holds "crash-$RUN_ID-$SEAT" "{\"show_id\": $SHOW_ID, \"seat_id\": $SEAT}"
  [ "$STATUS" = "201" ] || fail "hold seat $SEAT -> $STATUS"
  HOLDS="$HOLDS,$(json_field hold_id)"
done
HOLDS="${HOLDS#,}"
# The confirm requests "happened" a minute ago: PENDING payments whose charges
# went through at the gateway, but whose answers were lost. Holds are kept
# valid for 10 minutes so the reconciler can book them.
sql "UPDATE holds SET expires_at = NOW() + interval '10 minutes' WHERE id IN ($HOLDS)" > /dev/null
for KEY in $(sql "INSERT INTO payments (hold_id, user_id, amount_paise, created_at)
                  SELECT h.id, h.user_id, 25000, NOW() - interval '1 minute' FROM holds h WHERE h.id IN ($HOLDS)
                  RETURNING charge_key"); do
  curl -fsS -X POST "$MOCK_URL/charges" -d "{\"idempotency_key\":\"$KEY\",\"amount_paise\":25000}" > /dev/null
done
expect_sql "SELECT COUNT(*) FROM payments WHERE hold_id IN ($HOLDS) AND status = 'PENDING'" "$N" "PENDING payments in the backlog"
expect_mock money_taken_charges "$N" "charges taken at the gateway"

echo
echo "Step 2: start the worker and let it work for a few seconds ..."
$COMPOSE up -d worker > /dev/null 2>&1
for i in $(seq 1 20); do
  DONE_COUNT="$(sql "SELECT COUNT(*) FROM payments WHERE hold_id IN ($HOLDS) AND status = 'SUCCEEDED'" | tr -d '[:space:]')"
  [ "$DONE_COUNT" -ge 3 ] && break
  sleep 1
done

echo
echo "Step 3: KILL the worker mid-backlog (SIGKILL)"
$COMPOSE kill worker > /dev/null 2>&1
DONE_COUNT="$(sql "SELECT COUNT(*) FROM payments WHERE hold_id IN ($HOLDS) AND status = 'SUCCEEDED'" | tr -d '[:space:]')"
echo "  progress when killed: $DONE_COUNT of $N SUCCEEDED"
[ "$DONE_COUNT" -lt "$N" ] && pass "killed with work left ($((N - DONE_COUNT)) payments still PENDING)" \
                           || echo "  (note: the worker finished everything before the kill this time)"
sleep 2

echo
echo "Step 4: start the worker again; it picks up the rest (expired claims included) ..."
$COMPOSE up -d worker > /dev/null 2>&1
wait_for "SELECT COUNT(*) FROM payments WHERE hold_id IN ($HOLDS) AND status = 'SUCCEEDED'" "$N" 60 "SUCCEEDED payments"

echo
echo "Step 5: everything completed exactly once"
expect_sql "SELECT COUNT(*) FROM bookings WHERE hold_id IN ($HOLDS)" "$N" "bookings"
expect_sql "SELECT COUNT(*) FROM (SELECT hold_id FROM bookings WHERE hold_id IN ($HOLDS) GROUP BY hold_id HAVING COUNT(*) > 1) d" 0 "holds with more than one booking"
expect_mock money_taken_charges "$N" "still exactly $N charges (the worker never charges)"
echo "  reconcile attempts per payment: $(sql "SELECT string_agg(reconcile_attempts::text, ',' ORDER BY id) FROM payments WHERE hold_id IN ($HOLDS)")"
echo "  (an attempt of 2 = a row the killed worker had claimed; its lease expired and it was redone safely)"
mock_stats
run_verify
finish
