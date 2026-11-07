#!/usr/bin/env bash
# verify.sh - the ONE command that decides whether the code may be pushed.
#
#   gofmt (must list no files) -> go vet -> monitoring config (promtool)
#   -> unit tests -> integration tests
#
# Every line of output is also saved, UNFILTERED, to tmp/verify-<timestamp>.log
# (tmp/ is gitignored). Rule: only push when this script exits 0. When it
# fails, read the error lines in the log instead of a filtered summary.
#
# Why this script exists (Phase 2 lesson): a check like
#     make test-integration | grep ok && git push
# pushed failing code, because a pipeline's exit status is the status of its
# LAST command (grep), not of the tests. `set -o pipefail` makes a pipeline
# fail if ANY command in it fails; `set -e` stops at the first failure;
# `set -u` treats unset variables as errors.
#
# Usage:  scripts/verify.sh                    (needs `make up` for integration tests)
#         TEST_FLAGS=-race scripts/verify.sh   (race detector; needs cgo, used in CI)

set -euo pipefail
cd "$(dirname "$0")/.."

mkdir -p tmp
LOG="tmp/verify-$(date +%Y%m%d-%H%M%S).log"
# Send everything (stdout and stderr) to the screen AND the log file.
exec > >(tee "$LOG") 2>&1

trap 'echo; echo "VERIFY FAILED at line $LINENO (full log: $LOG)"' ERR

step() { echo; echo "==> $*"; }

step "gofmt -l (must be empty)"
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  echo "These files need gofmt:"
  echo "$unformatted"
  false
fi
echo "ok"

step "go vet"
go vet ./...
go vet -tags=integration ./...
echo "ok"

step "monitoring config: promtool check config / check rules / test rules"
# Same Prometheus image as docker-compose.yml, so promtool matches the server.
PROM_IMAGE="$(grep -oE 'prom/prometheus:v[0-9.]+' docker-compose.yml | head -n 1)"
if docker info > /dev/null 2>&1; then
  # Git Bash on Windows needs a Windows path for the volume and must not
  # rewrite "/etc/..." arguments; on Linux (CI) plain pwd is used.
  MON="$(pwd -W 2> /dev/null || pwd)/monitoring"
  export MSYS_NO_PATHCONV=1
  docker run --rm --entrypoint promtool -v "$MON:/etc/prometheus:ro" "$PROM_IMAGE" check config /etc/prometheus/prometheus.yml
  docker run --rm --entrypoint promtool -v "$MON:/etc/prometheus:ro" "$PROM_IMAGE" check rules /etc/prometheus/alerts.yml
  docker run --rm --entrypoint promtool -w /etc/prometheus -v "$MON:/etc/prometheus:ro" "$PROM_IMAGE" test rules alerts_test.yml
  unset MSYS_NO_PATHCONV
elif [ "${CI:-}" = "true" ]; then
  echo "Docker is required for the promtool checks in CI"
  false
else
  echo "SKIPPED: Docker is not available here (CI always runs these checks)"
fi
echo "(the dashboard JSON is validated by the Go test in monitoring/)"

step "unit tests"
make test TEST_FLAGS="${TEST_FLAGS:-}"

step "integration tests"
make test-integration TEST_FLAGS="${TEST_FLAGS:-}"

echo
echo "VERIFY OK (full log: $LOG)"
