#!/usr/bin/env bash
# race.sh - proves over real HTTP that a seat can only be booked once.
#
# It picks the first AVAILABLE seat of a show, then fires 20 booking requests
# for that same seat IN PARALLEL, each from a different user. Expected result:
#
#   201 Created : 1
#   409 Conflict: 19
#
# Finally it runs demo/verify.sql, which must return 0 rows (no seat has more
# than one booking).
#
# Usage:  ./demo/race.sh            (API must be running: make up && make seed)
#         API_URL=http://localhost:8080 SHOW_ID=1 REQUESTS=20 ./demo/race.sh

set -euo pipefail

API_URL="${API_URL:-http://localhost:8080}"
SHOW_ID="${SHOW_ID:-1}"
REQUESTS="${REQUESTS:-20}"

# Run from the repo root so the docker compose and file paths work.
cd "$(dirname "$0")/.."

# 1. Find the first AVAILABLE seat in the seat map (no jq needed).
#    The seat JSON looks like {"id":7,"label":"A7","price_paise":25000,"status":"AVAILABLE"}
SEAT_ID="$(curl -fsS "$API_URL/v1/shows/$SHOW_ID/seats" \
  | grep -oE '"id":[0-9]+,"label":"[^"]+","price_paise":[0-9]+,"status":"AVAILABLE"' \
  | head -n 1 | sed -E 's/"id":([0-9]+).*/\1/')"

if [ -z "$SEAT_ID" ]; then
  echo "No AVAILABLE seat left in show $SHOW_ID. Reset with: docker compose exec -T postgres psql -U seatlock -d seatlock -c 'TRUNCATE bookings'"
  exit 1
fi
echo "Firing $REQUESTS parallel booking requests for show $SHOW_ID, seat $SEAT_ID ..."

# 2. Fire all requests at once. Each curl runs in the background (&) and
#    writes only its HTTP status code into its own file.
RESULTS_DIR="$(mktemp -d)"
for i in $(seq 1 "$REQUESTS"); do
  curl -s -o /dev/null -w "%{http_code}" \
    -X POST "$API_URL/v1/bookings" \
    -H "Content-Type: application/json" \
    -H "X-User-ID: race-user-$i" \
    -d "{\"show_id\": $SHOW_ID, \"seat_id\": $SEAT_ID}" \
    > "$RESULTS_DIR/$i" &
done
wait # wait for every background curl to finish

# 3. Count the status codes.
CREATED="$(cat "$RESULTS_DIR"/* | grep -o "201" | wc -l | tr -d ' ')"
CONFLICT="$(cat "$RESULTS_DIR"/* | grep -o "409" | wc -l | tr -d ' ')"
OTHER=$((REQUESTS - CREATED - CONFLICT))
rm -f "$RESULTS_DIR"/*
rmdir "$RESULTS_DIR"

echo
echo "201 Created : $CREATED"
echo "409 Conflict: $CONFLICT"
if [ "$OTHER" -ne 0 ]; then
  echo "other       : $OTHER"
fi

# 4. Ask the database directly: is any seat booked more than once?
echo
echo "demo/verify.sql (must return 0 rows):"
docker compose exec -T postgres psql -U "${POSTGRES_USER:-seatlock}" -d "${POSTGRES_DB:-seatlock}" < demo/verify.sql

if [ "$CREATED" -eq 1 ] && [ "$CONFLICT" -eq $((REQUESTS - 1)) ]; then
  echo "PASS: exactly one booking won the race."
else
  echo "FAIL: expected 1 x 201 and $((REQUESTS - 1)) x 409."
  exit 1
fi
