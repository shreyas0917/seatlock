#!/usr/bin/env bash
# lib.sh - small helpers shared by the demo scripts. Not meant to be run directly:
#   source "$(dirname "$0")/lib.sh"

API_URL="${API_URL:-http://localhost:8080}"
SHOW_ID="${SHOW_ID:-1}"
PG_USER="${POSTGRES_USER:-seatlock}"
PG_DB="${POSTGRES_DB:-seatlock}"

FAILURES=0

pass() { echo "  PASS: $*"; }
fail() { echo "  FAIL: $*"; FAILURES=$((FAILURES + 1)); }

# request METHOD PATH USER BODY
# Sets two globals: STATUS (HTTP status code) and BODY (response body).
request() {
  local method="$1" path="$2" user="$3" body="${4:-}"
  local out
  out="$(curl -s -w $'\n%{http_code}' -X "$method" "$API_URL$path" \
    -H "Content-Type: application/json" \
    ${user:+-H "X-User-ID: $user"} \
    ${body:+-d "$body"})"
  STATUS="${out##*$'\n'}"
  BODY="${out%$'\n'*}"
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
