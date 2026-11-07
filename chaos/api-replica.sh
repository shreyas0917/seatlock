#!/usr/bin/env bash
# chaos/api-replica.sh — one of TWO api replicas is KILLED (SIGKILL) under load.
# Needs `make up-scale`: api + api-2 behind nginx (http://lb).
#
# Expected:
#   - the other replica keeps serving; nginx stops sending to the dead one
#   - requests that were IN FLIGHT on the killed replica fail (502): nginx
#     does not resend a POST that already reached a replica (no
#     `non_idempotent`, see deploy/lb/nginx.conf)
#   - the CLIENT (k6, CONFIRM_RETRIES=1) resends a failed confirm ONCE with
#     the SAME Idempotency-Key -> never a second charge
#   - a confirm whose answer was lost with the replica is resolved later
#     (Tx B, or the reconciler) -> consistent, 0 double charges
#   - the replica comes back and serves again
source "$(dirname "$0")/lib.sh"

if [ "$(docker compose --profile scale ps --status running --format '{{.Service}}' | grep -cE '^(api-2|lb)$')" != "2" ]; then
  echo "This chaos test needs two api replicas and the load balancer:  make up-scale"
  exit 2
fi
BASE_URL="http://lb"
prom_at() { curl -fsS -G "$PROM_URL/api/v1/query" --data-urlencode "query=$1" --data-urlencode "time=$2" \
  | grep -oE '"value":\[[0-9.e+]+,"[^"]*"\]' | head -n 1 | sed -E 's/.*,"([^"]*)"\]/\1/'; }

start_load "CONFIRM_RETRIES=1"
wait_until_t 30
expect_prom 'count(up{job="api"} == 1)' -eq 2 "Prometheus: 2 api replicas up before the kill"
before_t=$(date +%s)

docker compose --profile scale kill api-2 > /dev/null 2>&1
KILL_AT=$(date +%s)
event "api-2 killed (SIGKILL)"
wait_until_t 60
during_t=$(date +%s)
wait_until_t 75
docker compose --profile scale start api-2 > /dev/null 2>&1
event "api-2 started"
back=""
for _ in $(seq 1 60); do
  if [ "$(docker inspect -f '{{.State.Health.Status}}' "$(docker compose --profile scale ps -q api-2)" 2> /dev/null)" = "healthy" ]; then
    back=$(($(date +%s) - KILL_AT)); break
  fi
  sleep 1
done
[ -n "$back" ] && event "api-2 healthy again" || fail "api-2 not healthy within 60s of restarting"

finish_load

# The surviving replica kept serving: request rate while api-2 was dead vs before.
r_before="$(prom_at 'sum(rate(seatlock_http_requests_total{job="api"}[15s]))' "$before_t")"
r_during="$(prom_at 'sum(rate(seatlock_http_requests_total{job="api"}[15s]))' "$during_t")"
if awk -v a="$r_before" -v b="$r_during" 'BEGIN { exit !(a > 0 && b >= 0.8 * a) }'; then
  pass "the other replica kept serving: $(printf '%.0f' "$r_during") req/s while api-2 was dead ($(printf '%.0f' "$r_before") before)"
else
  fail "throughput dropped while api-2 was dead: '${r_during}' req/s vs '${r_before}' before"
fi
expect_prom 'count(up{job="api"} == 1)' -eq 2 "Prometheus: 2 api replicas up again"
served="$(prom_value "sum(increase(seatlock_http_requests_total{job=\"api\",instance=~\"$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$(docker compose --profile scale ps -q api-2)"):.*\"}[30s]))")"
if [ "${served%%.*}" -gt 0 ] 2> /dev/null; then pass "api-2 serves again after the restart (${served%%.*} requests in the last 30s)"
else fail "api-2 serves no requests after the restart ('$served')"; fi

r="$OUT_DIR/load.result.json"
retries="$(result_get "$r" counters.confirm_retries)"
unx="$(result_get "$r" counters.unexpected_status)"
echo "  users saw: ${unx:-0} failed requests in total (in flight on the killed replica); confirms resent with the same key: ${retries:-0}"

echo
echo "--- correctness after the kill (a booking may commit while its answer was lost: Postgres >= k6)"
correctness_checks "$OUT_DIR/load" "" "" atleast
chaos_done
