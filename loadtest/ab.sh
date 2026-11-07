#!/usr/bin/env bash
# loadtest/ab.sh — an INTERLEAVED before/after comparison of two api images:
#
#   PAIRS=3 RATE=800 loadtest/ab.sh seatlock-api:before seatlock-api:after
#
# Runs A, B, A, B, ... (one poolstats.sh run each), so slow drift on the
# machine (thermal throttling, background work, Docker VM state) hits both
# sides equally instead of making whichever ran last look worse.
# Each image is run as the api service (tagged seatlock-api, recreated).
set -u
cd "$(dirname "$0")/.." || exit 1
A="${1:?usage: loadtest/ab.sh IMAGE_A IMAGE_B}" B="${2:?usage: loadtest/ab.sh IMAGE_A IMAGE_B}"
PAIRS="${PAIRS:-3}" RATE="${RATE:-800}"
BASE="${OUT_DIR:-tmp/loadtest/ab-$RATE-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$BASE"
COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.bench.yml)

for p in $(seq 1 "$PAIRS"); do
  for side in A B; do
    img="$A"; [ "$side" = B ] && img="$B"
    docker tag "$img" seatlock-api:latest
    "${COMPOSE[@]}" up -d --no-build --force-recreate --wait api > /dev/null 2>&1 || { echo "api did not start with $img"; exit 1; }
    sleep 10
    RUNS=1 RATE="$RATE" OUT_DIR="$BASE/$side-$p" ./loadtest/poolstats.sh > "$BASE/$side-$p.out" 2>&1
    echo "pair $p $side ($img): $(tail -1 "$BASE/$side-$p.md")"
  done
done
docker tag "$B" seatlock-api:latest
"${COMPOSE[@]}" up -d --no-build --force-recreate --wait api > /dev/null 2>&1
for side in A B; do
  echo "== $side: median of $PAIRS runs"
  go run ./loadtest/report -median "$BASE"/"$side"-*/run1/run1.result.json | grep -E '^RPS|^(seats|hold|confirm) '
done
