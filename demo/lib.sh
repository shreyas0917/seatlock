#!/usr/bin/env bash
# lib.sh - small helpers shared by the demo scripts. Not meant to be run directly:
#   source "$(dirname "$0")/lib.sh"

API_URL="${API_URL:-http://localhost:8080}"
MOCK_URL="${MOCK_URL:-http://localhost:8095}"   # the mock payment gateway
PROM_URL="${PROM_URL:-http://localhost:9095}"   # Prometheus
GRAFANA_URL="${GRAFANA_URL:-http://localhost:3001}"
SHOW_ID="${SHOW_ID:-1}"
PG_USER="${POSTGRES_USER:-seatlock}"
PG_DB="${POSTGRES_DB:-seatlock}"

FAILURES=0

pass() { echo "  PASS: $*"; }
fail() { echo "  FAIL: $*"; FAILURES=$((FAILURES + 1)); }

# request METHOD PATH USER BODY [IDEMPOTENCY_KEY]
# Sets three globals: STATUS (HTTP status), BODY (response body) and
# REPLAYED ("true" if the response had Idempotent-Replayed: true).
request() {
  local method="$1" path="$2" user="$3" body="${4:-}" key="${5:-}"
  local headers out
  headers="$(mktemp)"
  out="$(curl -s -D "$headers" -w $'\n%{http_code}' -X "$method" "$API_URL$path" \
    -H "Content-Type: application/json" \
    ${user:+-H "X-User-ID: $user"} \
    ${key:+-H "Idempotency-Key: $key"} \
    ${body:+-d "$body"})"
  STATUS="${out##*$'\n'}"
  BODY="${out%$'\n'*}"
  if grep -qi '^Idempotent-Replayed: true' "$headers"; then REPLAYED=true; else REPLAYED=false; fi
  rm -f "$headers"
}

# expect_status WANT DESCRIPTION  - checks the STATUS of the last request.
expect_status() {
  if [ "$STATUS" = "$1" ]; then
    pass "$2 -> $STATUS"
  else
    fail "$2 -> expected $1, got $STATUS: $BODY"
  fi
}

# json_field NAME  - reads a numeric field from BODY (no jq needed).
json_field() {
  echo "$BODY" | grep -oE "\"$1\":[0-9]+" | head -n 1 | cut -d: -f2
}

# available_seats N  - prints the ids of the first N AVAILABLE seats of SHOW_ID.
# Seat JSON looks like {"id":7,"label":"A7","price_paise":25000,"status":"AVAILABLE"}
available_seats() {
  curl -fsS "$API_URL/v1/shows/$SHOW_ID/seats" \
    | grep -oE '"id":[0-9]+,"label":"[^"]+","price_paise":[0-9]+,"status":"AVAILABLE"' \
    | head -n "$1" | sed -E 's/"id":([0-9]+).*/\1/'
}

# sql "QUERY"  - runs one SQL query and prints the bare result.
sql() {
  docker compose exec -T postgres psql -q -U "$PG_USER" -d "$PG_DB" -tAc "$1"
}

# expect_sql "QUERY" WANT DESCRIPTION  - checks a single-value SQL result.
expect_sql() {
  local got
  got="$(sql "$1" | tr -d '[:space:]')"
  if [ "$got" = "$2" ]; then pass "$3 = $got"; else fail "$3: expected $2, got '$got'"; fi
}

# --- mock payment gateway helpers ---

# mock_reset - forget all charges, refunds, stats and queued behaviours.
mock_reset() {
  curl -fsS -X POST "$MOCK_URL/debug/reset" > /dev/null || fail "could not reset the mock gateway at $MOCK_URL"
}

# mock_queue BEHAVIOR  - the next NEW charge will be ok|decline|slow|timeout.
mock_queue() {
  curl -fsS -X POST "$MOCK_URL/debug/next-behavior" -d "{\"behavior\":\"$1\",\"count\":1}" > /dev/null \
    || fail "could not queue behaviour $1"
}

# mock_stats - prints the gateway's counters (charges, refunds, per key).
mock_stats() {
  echo
  echo "Gateway stats: $(curl -fsS "$MOCK_URL/debug/stats")"
}

# mock_stat FIELD  - prints one numeric field of the stats, e.g. total_charges.
mock_stat() {
  curl -fsS "$MOCK_URL/debug/stats" | grep -oE "\"$1\":[0-9]+" | cut -d: -f2
}

# expect_mock FIELD WANT DESCRIPTION
expect_mock() {
  local got
  got="$(mock_stat "$1")"
  if [ "$got" = "$2" ]; then pass "$3 ($1 = $got)"; else fail "$3: expected $1 = $2, got '$got'"; fi
}

# require_demo_mode - the worker demos need the short timings of
# docker-compose.demo.yml (`make up-demo`); stop with a clear hint otherwise.
require_demo_mode() {
  if [ "$(docker compose exec -T worker printenv DEMO_MODE 2>/dev/null | tr -d '[:space:]')" != "true" ]; then
    echo "This demo needs the short demo timings. Start the stack with:  make up-demo"
    exit 2
  fi
}

# wait_for "SQL" WANT TIMEOUT_SECONDS DESCRIPTION - polls a single-value SQL
# query once per second until it returns WANT. Prints how long it took.
wait_for() {
  local query="$1" want="$2" limit="$3" what="$4" got waited=0
  while [ "$waited" -lt "$limit" ]; do
    got="$(sql "$query" | tr -d '[:space:]')"
    if [ "$got" = "$want" ]; then
      pass "$what = $want (after ${waited}s)"
      return 0
    fi
    sleep 1
    waited=$((waited + 1))
  done
  fail "$what: expected $want within ${limit}s, last value '$got'"
  return 1
}

# redis_has KEY - prints 1 if the key exists in Redis (app database 0), else 0.
redis_has() {
  docker compose exec -T redis redis-cli exists "$1" | tr -d '[:space:]'
}

# prom_value "PROMQL" - prints the value of the FIRST series of an instant
# query ("" if the result is empty). No jq needed.
prom_value() {
  curl -fsS -G "$PROM_URL/api/v1/query" --data-urlencode "query=$1" \
    | grep -oE '"value":\[[0-9.e+]+,"[^"]*"\]' | head -n 1 | sed -E 's/.*,"([^"]*)"\]/\1/'
}

# expect_prom "PROMQL" OP WANT DESCRIPTION - compares a query's value with
# WANT using OP (-eq, -ge, ...). Values are rounded down to whole numbers.
expect_prom() {
  local got
  got="$(prom_value "$1")"
  got="${got%%.*}"
  if [ -n "$got" ] && [ "$got" "$2" "$3" ]; then
    pass "$4 ($got)"
  else
    fail "$4: expected $2 $3, got '${got:-no data}'  [query: $1]"
  fi
}

# run_verify - runs demo/verify.sql. Every check in it must return 0 rows.
run_verify() {
  echo
  echo "demo/verify.sql (every check must return 0 rows):"
  local out
  out="$(docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U "$PG_USER" -d "$PG_DB" < demo/verify.sql)"
  echo "$out"
  local checks zero
  checks="$(echo "$out" | grep -c '^CHECK')"
  zero="$(echo "$out" | grep -c '^(0 rows)')"
  if [ "$checks" -gt 0 ] && [ "$checks" = "$zero" ]; then
    pass "verify.sql: all $checks checks returned 0 rows"
  else
    fail "verify.sql: only $zero of $checks checks returned 0 rows"
  fi
}

# finish - prints the overall result and exits non-zero if anything failed.
finish() {
  echo
  if [ "$FAILURES" -eq 0 ]; then
    echo "RESULT: PASS"
    exit 0
  fi
  echo "RESULT: FAIL ($FAILURES failed checks)"
  exit 1
}
