#!/usr/bin/env bash
# docs/images/src/screenshots.sh — regenerates the README images from REAL runs
# (make screenshots). Takes ~10 minutes; needs the stack built (make up) and
# the load data (make seed-load).
#
#   1. worker demos     (make up-demo timings)  -> dashboard-worker.png window
#   2. request demos    (normal timings)        -> dashboard-overview.png window
#   3. chaos/redis-down.sh under load (bench)   -> dashboard-fail-closed.png window
#   4. capture.cjs: headless Chromium (Playwright in Docker), 1920x1080, Grafana
#      kiosk, dark theme; fails if any captured panel shows "No data", an error
#      or is still loading. Then render.cjs: the diagram and social preview.
#   5. back to the normal stack (make up).
# Never edit or crop the output; if a shot fails its checks, re-run.
set -euo pipefail
cd "$(dirname "$0")/../../.."

PW_IMAGE="mcr.microsoft.com/playwright:v1.49.1-noble"
WORK="tmp/screenshots"
mkdir -p "$WORK/nm"
export MSYS_NO_PATHCONV=1
HOST="$(pwd -W 2> /dev/null || pwd)"
compose() { docker compose "$@" > /dev/null; }

# playwright-core for the scripts, installed once into tmp/ (ignored by git).
if [ ! -d "$WORK/nm/node_modules/playwright-core" ]; then
  docker run --rm -v "$HOST/$WORK/nm:/nm" "$PW_IMAGE" sh -c \
    'cd /nm && npm init -y > /dev/null && npm i --no-audit --no-fund playwright-core@1.49.1 > /dev/null'
fi

run_pw() {
  docker run --rm --network seatlock_default -v "$HOST/$WORK/nm/node_modules:/node_modules:ro" \
    -v "$HOST/docs/images/src:/src:ro" -v "$HOST/docs/images:/out" -e NODE_PATH=/node_modules \
    -e WORKER_FROM="${WORKER_FROM:-}" -e WORKER_TO="${WORKER_TO:-}" -e REQ_FROM="${REQ_FROM:-}" -e REQ_TO="${REQ_TO:-}" \
    -e FAIL_FROM="${FAIL_FROM:-}" -e FAIL_TO="${FAIL_TO:-}" "$PW_IMAGE" node "$@"
}

# make images: only re-render the diagram and the social preview.
if [ "${1:-}" = "--render-only" ]; then
  run_pw /src/render.cjs
  exit 0
fi

echo "1. worker demos"
compose -f docker-compose.yml -f docker-compose.demo.yml up -d --no-build --wait
make -s reset-data > /dev/null
WORKER_FROM=$(date +%s)
for d in worker-reconcile outbox-retry abandon; do ./demo/$d.sh > "$WORK/$d.log"; done
WORKER_TO=$(date +%s)

echo "2. request-path demos"
compose up -d --no-build --wait
sleep 20
make -s reset-data > /dev/null
REQ_FROM=$(date +%s)
./demo/race.sh > "$WORK/race.log"
make -s reset-data > /dev/null
./demo/observability.sh > "$WORK/observability.log"
REQ_TO=$(date +%s)

echo "3. chaos/redis-down.sh under load"
compose -f docker-compose.yml -f docker-compose.bench.yml up -d --no-build --wait
make -s seed-load > /dev/null
CHAOS_DIR="$WORK/chaos-$(date +%Y%m%d-%H%M%S)"
OUT_DIR="$CHAOS_DIR" CHAOS_RATE=200 ./chaos/redis-down.sh > "$WORK/chaos.log"
FAIL_FROM=$(head -1 "$CHAOS_DIR/events.txt" | cut -d' ' -f1)
FAIL_TO=$(tail -1 "$CHAOS_DIR/events.txt" | cut -d' ' -f1)

echo "4. capture + render"
run_pw /src/capture.cjs
run_pw /src/render.cjs

echo "5. back to the normal stack"
compose up -d --no-build --wait
ls -la docs/images/*.png
