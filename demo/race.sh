#!/usr/bin/env bash
# race.sh - proves over real HTTP that a seat can only be held (and booked) once.
#
#   1. picks the first AVAILABLE seat of the show
#   2. 20 different users try to HOLD it at the same moment
#        expected: 201 Created : 1   409 Conflict: 19
#   3. the winner CONFIRMS its hold
#        expected: 201
#   4. runs demo/verify.sql: every invariant check must return 0 rows
#
# Usage:  ./demo/race.sh            (API must be running: make up && make seed)
#         API_URL=http://localhost:8080 SHOW_ID=1 REQUESTS=20 ./demo/race.sh

set -uo pipefail
cd "$(dirname "$0")/.."   # repo root, so docker compose and paths work
source demo/lib.sh

REQUESTS="${REQUESTS:-20}"
RUN_ID="$(date +%s)"       # makes user ids unique per run

SEAT_ID="$(available_seats 1)"
if [ -z "$SEAT_ID" ]; then
  echo "No AVAILABLE seat left in show $SHOW_ID."
  echo "Reset with: docker compose exec -T postgres psql -U seatlock -d seatlock -c 'TRUNCATE bookings, holds'"
  exit 1
fi
echo "Step 1: $REQUESTS users try to HOLD show $SHOW_ID, seat $SEAT_ID at the same moment ..."

# Fire all requests at once. Each curl runs in the background (&) and writes
# its status code and body into its own files.
RESULTS_DIR="$(mktemp -d)"
for i in $(seq 1 "$REQUESTS"); do
  curl -s -o "$RESULTS_DIR/$i.body" -w "%{http_code}" \
    -X POST "$API_URL/v1/holds" \
    -H "Content-Type: application/json" \
    -H "X-User-ID: race-$RUN_ID-user-$i" \
    -d "{\"show_id\": $SHOW_ID, \"seat_id\": $SEAT_ID}" \
    > "$RESULTS_DIR/$i.code" &
done
wait # wait for every background curl to finish

CREATED=0; CONFLICT=0; OTHER=0; WINNER=""
for i in $(seq 1 "$REQUESTS"); do
  case "$(cat "$RESULTS_DIR/$i.code")" in
    201) CREATED=$((CREATED + 1)); WINNER="$i" ;;
    409) CONFLICT=$((CONFLICT + 1)) ;;
    *)   OTHER=$((OTHER + 1)); echo "  unexpected: user $i got $(cat "$RESULTS_DIR/$i.code"): $(cat "$RESULTS_DIR/$i.body")" ;;
  esac
done

echo
echo "201 Created : $CREATED"
echo "409 Conflict: $CONFLICT"
[ "$OTHER" -ne 0 ] && echo "other       : $OTHER"

if [ "$CREATED" -eq 1 ] && [ "$CONFLICT" -eq $((REQUESTS - 1)) ]; then
  pass "exactly one user won the hold"
else
  fail "expected 1 x 201 and $((REQUESTS - 1)) x 409"
fi

if [ -n "$WINNER" ]; then
  BODY="$(cat "$RESULTS_DIR/$WINNER.body")"
  HOLD_ID="$(json_field hold_id)"
  echo
  echo "Step 2: the winner (race-$RUN_ID-user-$WINNER) confirms hold $HOLD_ID ..."
  request POST /v1/bookings "race-$RUN_ID-user-$WINNER" "{\"hold_id\": $HOLD_ID}"
  echo "  $BODY"
  expect_status 201 "winner confirms its hold"
fi

rm -f "$RESULTS_DIR"/*
rmdir "$RESULTS_DIR"

run_verify
finish
