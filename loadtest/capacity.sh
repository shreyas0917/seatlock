#!/usr/bin/env bash
# loadtest/capacity.sh — the capacity ladder: runs capacity.js at increasing
# arrival rates (each step = loadtest/run.sh capacity, RUNS times) and prints
# the step table and the KNEE:
#
#   knee = the highest step where (medians of the runs)
#            p95(hold) < 250 ms  AND  shed + unexpected < 0.1%
#          AND k6 delivered the rate (dropped_iterations == 0)
#   A 500 at ANY step is a bug: the step is marked BUG and the script fails.
#
#   RUNS=3 CAP_STEPS="100 200 400 800" loadtest/capacity.sh
#
# It stops after CAP_STOP_AFTER (default 2) steps in a row past the knee.
set -u
cd "$(dirname "$0")/.." || exit 1

RUNS="${RUNS:-3}"
STEPS="${CAP_STEPS:-100 200 400 600 800 1000 1300 1600 2000}"
STOP_AFTER="${CAP_STOP_AFTER:-2}"
DURATION="${CAP_DURATION:-60s}"
BASE_DIR="${OUT_DIR:-tmp/loadtest/capacity-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$BASE_DIR"

get() { go run ./loadtest/report -get "$1" "$2"; }
# median of the values on stdin (one per line)
median() { sort -g | awk '{ v[NR] = $1 } END { if (NR == 0) print ""; else if (NR % 2) print v[(NR + 1) / 2]; else print (v[NR / 2] + v[NR / 2 + 1]) / 2 }'; }
med() { for f in "$BASE_DIR/step-$1"/run*.result.json; do get "$2" "$f"; done | grep -v '^$' | median; }

TABLE="$BASE_DIR/steps.md"
{
  echo "| Target rate (actions/s) | Achieved RPS | p50 / p95 / p99 hold (ms) | p95 seat map (ms) | p95 confirm (ms) | p95 cancel (ms) | Shed % | Unexpected % | Dropped | 500s | Within the knee rule? |"
  echo "|---|---|---|---|---|---|---|---|---|---|---|"
} > "$TABLE"

knee="" past=0 bug=0
for rate in $STEPS; do
  echo "################ step $rate actions/s ($RUNS runs)"
  OUT_DIR="$BASE_DIR/step-$rate" RUNS="$RUNS" K6_ENV="CAP_RATE=$rate CAP_DURATION=$DURATION ${K6_ENV:-}" \
    ./loadtest/run.sh capacity > "$BASE_DIR/step-$rate.log" 2>&1
  tail -3 "$BASE_DIR/step-$rate.log"

  rps="$(med "$rate" rps)"
  h50="$(med "$rate" endpoints.hold.p50_ms)" h95="$(med "$rate" endpoints.hold.p95_ms)" h99="$(med "$rate" endpoints.hold.p99_ms)"
  s95="$(med "$rate" endpoints.seats.p95_ms)" c95="$(med "$rate" endpoints.confirm.p95_ms)" x95="$(med "$rate" endpoints.cancel.p95_ms)"
  shed="$(med "$rate" shed_pct)" unx="$(med "$rate" unexpected_pct)"
  dropped="$(med "$rate" counters.dropped_iterations)"
  e500="$(for f in "$BASE_DIR/step-$rate"/run*.result.json; do get counters.status_500 "$f"; done | awk '{ s += $1 } END { print s + 0 }')"
  # The run.sh correctness checks (verify.sql, gateway, invariants) must pass at every step.
  correct="$(grep -c PASS "$BASE_DIR/step-$rate"/run*.verdict 2>/dev/null | awk -F: '{ s += $NF } END { print s + 0 }')"

  verdict="no"
  if [ "$e500" -gt 0 ]; then
    verdict="**BUG: $e500 × 500**"; bug=1
  elif awk -v p="$h95" -v s="$shed" -v u="$unx" -v d="$dropped" 'BEGIN { exit !(p != "" && p < 250 && s + u < 0.1 && d == 0) }'; then
    verdict="yes"
  fi
  [ "$correct" -lt "$RUNS" ] && verdict="$verdict (correctness check failed: see step-$rate.log)" && bug=1

  printf '| %s | %.0f | %.1f / %.1f / %.1f | %.1f | %.1f | %.1f | %.3f | %.3f | %s | %s | %s |\n' \
    "$rate" "$rps" "$h50" "$h95" "$h99" "${s95:-0}" "${c95:-0}" "${x95:-0}" "$shed" "$unx" "$dropped" "$e500" "$verdict" >> "$TABLE"
  tail -1 "$TABLE"

  if [ "$verdict" = "yes" ]; then knee="$rate"; past=0; else past=$((past + 1)); fi
  [ "$past" -ge "$STOP_AFTER" ] && { echo "stopping: $past steps in a row outside the knee rule"; break; }
done

{
  echo
  if [ -n "$knee" ]; then echo "**Knee: $knee actions/s** (highest step with p95(hold) < 250 ms and shed + unexpected < 0.1%; medians of $RUNS runs)"
  else echo "**No step met the knee rule.**"; fi
} >> "$TABLE"
echo
cat "$TABLE"
[ "$bug" -eq 0 ]
