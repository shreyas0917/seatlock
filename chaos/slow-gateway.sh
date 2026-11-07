#!/usr/bin/env bash
# chaos/slow-gateway.sh — the payment gateway answers every charge after
# SLOW_DELAY (default 3 s) for 60 s, under load (mock POST /debug/slow).
#
# Expected:
#   - confirms slow down (p95 about the delay); with PAYMENT_TIMEOUT=5s a 3 s
#     answer still arrives in time, so most succeed. Any that time out are
#     504 PAYMENT_PENDING and are reconciled later.
#   - seat maps and holds are NOT slowed down: a confirm holds no DB
#     connection while it waits for the gateway (Tx A -> gateway -> Tx B)
#   - every payment ends booked or refunded, 0 double charges
source "$(dirname "$0")/lib.sh"
SLOW_DELAY="${SLOW_DELAY:-3s}"
prom_at() { curl -fsS -G "$PROM_URL/api/v1/query" --data-urlencode "query=$1" --data-urlencode "time=$2" \
  | grep -oE '"value":\[[0-9.e+]+,"[^"]*"\]' | head -n 1 | sed -E 's/.*,"([^"]*)"\]/\1/'; }
p95() { prom_at "histogram_quantile(0.95, sum by (le) (rate(seatlock_http_request_duration_seconds_bucket{route=\"$1\"}[20s]))) * 1000" "$2"; }

start_load
wait_until_t 30
before_t=$(date +%s)
curl -fsS -X POST "$MOCK_URL/debug/slow" -d "{\"delay\":\"$SLOW_DELAY\",\"for\":\"60s\"}" > /dev/null || fail "could not slow the gateway"
event "gateway slow ($SLOW_DELAY per charge) for 60s"
wait_until_t 80
during_t=$(date +%s)
wait_until_t 90
event "gateway back to normal"

finish_load
c_before="$(p95 /v1/bookings "$before_t")" c_during="$(p95 /v1/bookings "$during_t")"
h_before="$(p95 /v1/holds "$before_t")" h_during="$(p95 /v1/holds "$during_t")"
s_before="$(p95 '/v1/shows/{showID}/seats' "$before_t")" s_during="$(p95 '/v1/shows/{showID}/seats' "$during_t")"
printf '  p95 before -> during the slowdown: confirm %.0f -> %.0f ms, hold %.0f -> %.0f ms, seat map %.0f -> %.0f ms\n' \
  "$c_before" "$c_during" "$h_before" "$h_during" "$s_before" "$s_during"
if awk -v d="$c_during" 'BEGIN { exit !(d >= 2500) }'; then pass "confirms slowed down with the gateway (p95 $(printf '%.0f' "$c_during") ms)"
else fail "confirms did not slow down: p95 '${c_during}' ms"; fi
if awk -v a="$h_before" -v b="$h_during" 'BEGIN { exit !(b <= 2 * a + 20) }'; then pass "holds were not slowed down by the slow gateway (no DB connection held during the charge)"
else fail "holds slowed down too: p95 $h_before -> $h_during ms"; fi
t504="$(result_get "$OUT_DIR/load.result.json" counters.confirm_504)"
echo "  users saw ${t504:-0} x 504 on confirm (outcome unknown; reconciled later)"

echo
echo "--- correctness after the slowdown"
correctness_checks "$OUT_DIR/load" "" "" atleast
chaos_done
