#!/usr/bin/env bash
# lib.sh - small helpers shared by the demo scripts. Not meant to be run directly:
#   source "$(dirname "$0")/lib.sh"

API_URL="${API_URL:-http://localhost:8080}"
MOCK_URL="${MOCK_URL:-http://localhost:8095}"   # the mock payment gateway
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
  docker compose exec -T postgres psql -U "$PG_USER" -d "$PG_DB" -tAc "$1"
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
