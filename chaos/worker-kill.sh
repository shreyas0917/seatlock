#!/usr/bin/env bash
# chaos/worker-kill.sh — the worker is KILLED (SIGKILL) under load while a
# backlog builds up, and restarted 60 s later.
#
# The backlog: 10 charges are made to TIME OUT at the gateway (money taken,
# answer lost): those confirms end 504 PAYMENT_PENDING and leave PENDING
# payments that only the worker's reconciler can resolve; cancels keep
# adding RELEASE_LOCK outbox rows for the drainer. With the worker dead,
# nothing resolves them.
#
# Expected: after the restart the backlog drains by itself; every PENDING
# payment ends up booked or refunded EXACTLY once (0 double charges, at most
# one refund per charge), no outbox row is left PENDING or FAILED, and
# verify.sql + invariants are 0.
source "$(dirname "$0")/lib.sh"

start_load
wait_until_t 30
docker compose kill worker > /dev/null 2>&1
event "worker killed (SIGKILL)"
curl -fsS -X POST "$MOCK_URL/debug/next-behavior" -d '{"behavior":"timeout","count":10}' > /dev/null || fail "could not queue gateway timeouts"
event "next 10 charges will time out"

wait_until_t 90
pend_pay="$(sql "SELECT COUNT(*) FROM payments WHERE status = 'PENDING'" | tr -d '[:space:]')"
pend_out="$(sql "SELECT COUNT(*) FROM outbox WHERE status = 'PENDING'" | tr -d '[:space:]')"
echo "  backlog with the worker dead: $pend_pay PENDING payments, $pend_out PENDING outbox rows"
[ "$pend_pay" -gt 0 ] && pass "a backlog of PENDING payments built up ($pend_pay)" || fail "no PENDING payments built up"
docker compose start worker > /dev/null 2>&1
START_AT=$(date +%s)
event "worker started"

finish_load
t504="$(result_get "$OUT_DIR/load.result.json" counters.confirm_504)"
echo "  users saw ${t504:-0} x 504 (payment outcome unknown; resolved later by the reconciler)"

echo
echo "--- the backlog drains (the reconciler waits RECONCILE_AFTER=2m before touching a payment)"
CONSISTENCY_TIMEOUT=420 correctness_checks "$OUT_DIR/load" "" "" atleast
echo "  backlog drained $(($(date +%s) - START_AT))s after the worker restarted (including the checks)"
expect_sql "SELECT COUNT(*) FROM outbox WHERE status <> 'DONE'" 0 "outbox rows not DONE (PENDING or FAILED)"
expect_sql "SELECT COUNT(*) FROM payments WHERE status NOT IN ('SUCCEEDED', 'DECLINED', 'REFUNDED', 'ABANDONED')" 0 "payments not in a final state"
chaos_done
