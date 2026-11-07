#!/usr/bin/env bash
# observability.sh - proves the metrics, alerts and dashboard see what happens.
#
#   1. every Prometheus scrape target (api, worker, payment mock, prometheus) is "up"
#   2. traffic: race.sh (20 users race for one seat), payment-timeout.sh
#      (money taken, answer lost, retry) and a brief Redis stop/start
#      (new holds fail closed with 503)
#   3. Prometheus is asked, and must show:
#        holds created >= 1, seat_held >= 19, lock_unavailable >= 1,
#        all 11 invariant gauges reporting and every one = 0,
#        no DoubleBookingDetected alert firing
#   4. prints the Grafana dashboard URL (for screenshots: docs/images/README.md)
#
# Usage:  make up && make seed && ./demo/observability.sh      (about 1 minute)

set -uo pipefail
cd "$(dirname "$0")/.."
source demo/lib.sh

echo "Step 1: Prometheus scrape targets"
TARGETS="$(curl -fsS "$PROM_URL/api/v1/targets?state=active")" || { echo "Prometheus is not reachable at $PROM_URL (make up)"; exit 1; }
UP="$(echo "$TARGETS" | grep -oE '"health":"up"' | wc -l | tr -d ' ')"
NOT_UP="$(echo "$TARGETS" | grep -oE '"health":"(down|unknown)"' | wc -l | tr -d ' ')"
for job in api worker payment-mock prometheus; do
  if echo "$TARGETS" | grep -q "\"job\":\"$job\""; then pass "target $job is scraped"; else fail "target $job is missing"; fi
done
[ "$NOT_UP" -eq 0 ] && [ "$UP" -ge 4 ] && pass "all $UP targets are up" || fail "$NOT_UP target(s) not up"

echo
echo "Step 2: traffic"
echo "  - race.sh (20 users, one seat) ..."
./demo/race.sh > /dev/null 2>&1 && pass "race.sh passed" || fail "race.sh failed (run it alone to see why)"
echo "  - payment-timeout.sh (money taken, answer lost, retry) ..."
./demo/payment-timeout.sh > /dev/null 2>&1 && pass "payment-timeout.sh passed" || fail "payment-timeout.sh failed"
echo "  - Redis stop / hold attempt / start ..."
trap 'docker compose start redis > /dev/null 2>&1' EXIT
SEAT="$(available_seats 1)"
docker compose stop redis > /dev/null 2>&1
request POST /v1/holds "obs-$(date +%s)" "{\"show_id\": $SHOW_ID, \"seat_id\": $SEAT}"
expect_status 503 "hold while Redis is down fails closed"
docker compose up -d --wait redis > /dev/null 2>&1 && pass "Redis is back"

echo
echo "Step 3: what Prometheus saw (waiting 20s for scrapes and one invariants run) ..."
sleep 20
expect_prom 'sum(seatlock_holds_total{outcome="created"})' -ge 1 "holds created"
expect_prom 'sum(seatlock_holds_total{outcome="seat_held"})' -ge 19 "holds that lost the race (seat_held)"
expect_prom 'sum(seatlock_holds_total{outcome="lock_unavailable"})' -ge 1 "holds refused while Redis was down (lock_unavailable)"
expect_prom 'sum(seatlock_redis_lock_ops_total{result="error"})' -ge 1 "Redis lock errors recorded (fail closed)"
expect_prom 'sum(seatlock_confirms_total{outcome="pending"})' -ge 1 "confirms with an unknown payment outcome (504)"
expect_prom 'count(max by (check) (seatlock_invariant_violations))' -eq 11 "invariant checks reporting"
expect_prom 'max(seatlock_invariant_violations)' -eq 0 "largest invariant violation count"
FIRING="$(curl -fsS -G "$PROM_URL/api/v1/query" --data-urlencode 'query=ALERTS{alertname="DoubleBookingDetected",alertstate="firing"}')"
if echo "$FIRING" | grep -q '"result":\[\]'; then pass "DoubleBookingDetected is not firing"; else fail "DoubleBookingDetected is FIRING: $FIRING"; fi
echo "  alerts firing right now: $(curl -fsS -G "$PROM_URL/api/v1/query" --data-urlencode 'query=ALERTS{alertstate="firing"}' | grep -oE '"alertname":"[^"]+"' | sort -u | tr '\n' ' ')"
echo "  (RedisLockUnavailable may be firing for a minute after the Redis stop: that is the alert working.)"

echo
echo "Step 4: open the dashboard"
echo "  Grafana:    $GRAFANA_URL/d/seatlock-reliability   (no login needed)"
echo "  Prometheus: $PROM_URL/alerts"

run_verify
finish
