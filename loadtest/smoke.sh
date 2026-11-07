#!/usr/bin/env bash
# loadtest/smoke.sh — the k6 smoke test (CI's "Load smoke" workflow, and
# `VERIFY_LOAD=1 scripts/verify.sh` locally). About 1 minute.
#
#   200 booking attempts on 20 seats -> exactly 20 bookings, 0 unexpected
#   statuses, gateway charges == bookings + refunds, verify.sql 0 rows.
#   Any double booking or invariant violation fails it (exit 1).
#
# Needs the stack running (`make up` locally; CI starts postgres, redis,
# payment, api and worker). Prometheus is optional: without it (CI) the
# invariant-gauge check is skipped, verify.sql still runs.
set -euo pipefail
cd "$(dirname "$0")/.."

make -s seed-load > /dev/null
if ! curl -fsS -m 3 "${PROM_URL:-http://localhost:9095}/-/ready" > /dev/null 2>&1; then
  export NO_PROM=1
fi
RUNS=1 SKIP_WARMUP=1 OUT_DIR="${OUT_DIR:-tmp/loadtest/smoke-$(date +%Y%m%d-%H%M%S)}" ./loadtest/run.sh smoke
