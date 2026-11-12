# Testing, demos, load tests and chaos

How SeatLock is verified, from unit tests to chaos under load. Results of the load and chaos runs: [benchmarks/RESULTS.md](benchmarks/RESULTS.md).

## The push gate

```bash
make test               # unit + handler tests (no database, Redis or gateway)
make test-integration   # + real Postgres, Redis and the in-process mock gateway (needs `make up`)
make verify             # the gate: gofmt, vet, link check, promtool, unit + integration tests; full log in tmp/
VERIFY_LOAD=1 make verify   # + the k6 smoke test (~1 min, needs `make up`)
```

`scripts/verify.sh` is the **single gate**: nothing is pushed unless it exits 0. It saves the full, unfiltered log to `tmp/verify-<timestamp>.log`.

| Where | What runs |
|---|---|
| `scripts/verify.sh` (local) | gofmt, `go vet` (also with the integration tag), offline markdown link check, `promtool check config / check rules / test rules`, unit tests, integration tests; optional k6 smoke (`VERIFY_LOAD=1`). The Docker-based checks are skipped without Docker |
| CI workflow ([ci.yml](../.github/workflows/ci.yml)) | the same `verify.sh` **with `-race`**, against Postgres 16 and Redis 7 service containers; the Docker-based checks are **required** there |
| Load smoke workflow ([load-smoke.yml](../.github/workflows/load-smoke.yml)) | starts the real Compose stack and sends 200 booking attempts at 20 seats; **fails the build** unless exactly 20 bookings, 0 unexpected statuses, gateway charges == bookings + refunds and `verify.sql` returns 0 rows |

Integration tests use the `seatlock_test` database and Redis database 1, so they never touch your demo data. Always run them through `make test-integration` / `make verify` (`-p 1`: packages share the test database).

## Key proofs (integration tests)

| Test | Proves |
|---|---|
| `TestHoldRace` | 50 users, same seat → **1 hold, 49 `ErrSeatHeld`** |
| `TestRedisLostData_PostgresWins` | Redis `FLUSHDB` → B gets the Redis lock but **Postgres rejects B** |
| `TestPay_SameKeyConcurrent_OneChargeOneBooking` | same key, 10 concurrent → **1 charge, 1 booking** |
| `TestPay_TimeoutThenRetrySameKey_OneCharge` | lost answer → 504; retry → 201, **still 1 charge** |
| `TestPay_HoldExpiresDuringPayment_Refunded` | hold expires mid-payment → **410, refunded, no booking** |
| `TestReconciler_ChargeSucceeded_HoldValid_Books` | 504 never retried → **the worker books it**, 1 charge |
| `TestReconciler_NotFound_VoidsAbandons_ThenNewPaymentWorks` | no charge → key voided → `ABANDONED`; late charge refused; new payment → **1 successful charge total** |
| `TestReconciler_VoidRace_LateChargeWins_Books` | the late charge beats the void → booked, nothing lost |
| `TestDrainer_BackoffThenSuccess` / `TestDrainer_DeadLetter` | refunds retry with growing delays; after N attempts → `FAILED`, not retried |
| `TestPendingHoldMax_SeatFreesThenLateSuccessIsRefunded` | gateway down for long → seat frees after `PENDING_HOLD_MAX`; late success → **refund, no booking** |
| `TestTwoWorkers_ProcessEveryRowExactlyOnce` | 2 workers, 30-row backlog → **every row processed exactly once**, 30 refunds |
| `TestInvariants_DeliberateViolationIsReported` | a booking nobody paid for → its invariant gauge becomes 1 (the alert can fire) |
| `TestInvariants_FailingCheckKeepsLastValueAndCountsError` | a timed-out check keeps its last value and counts an error: never a false 0 |
| `TestDBError_UnavailablePostgresBecomesErrBusy` | every error kind seen during a Postgres restart maps to 503, real bugs stay 500 |
| `TestMetrics_RouteLabelIsThePattern` | `/v1/holds/123` is recorded as `/v1/holds/{holdID}`: no ids in labels |
| `alerts_test.yml` (promtool) | `DoubleBookingDetected`, `WorkerJobStalled` (stalled vs healthy) and `OutboxDeadLetters` fire exactly when they should |

## Demos (against the running stack)

```bash
make reset-data                      # free all seats (keeps shows/seats; clears Redis seat locks too)

# request path (work with `make up` or `make up-demo`)
./demo/race.sh                       # 20 users race to hold one seat -> 201: 1, 409: 19; winner pays -> 201
./demo/redis-down.sh                 # Redis stopped: new hold 503, existing hold pays + books 201, recovery
./demo/duplicate-request.sh          # same confirm + same key 5x -> 1 booking, 1 charge, 4 identical replays
./demo/payment-timeout.sh            # money taken, answer lost -> 504 -> retry same key -> 201, still 1 charge
./demo/expired-during-payment.sh     # hold expires mid-payment -> 410, refunded, no booking

# the worker (needs the short timings: `make up-demo`)
./demo/abandon.sh                    # hold, walk away -> worker expires it + releases the Redis lock -> another user holds it
./demo/worker-reconcile.sh           # 504 and the user never retries -> worker books it, 1 charge
./demo/outbox-retry.sh               # refund fails twice -> worker retries with backoff -> REFUNDED, 1 refund
./demo/worker-crash.sh               # worker SIGKILLed mid-backlog, restarted -> all 12 completed exactly once

# observability (with `make up`)
./demo/observability.sh              # targets up, traffic + Redis stop, then asserts what Prometheus saw
```

Every demo prints PASS/FAIL per step and the gateway's stats, exits non-zero on any failure, and ends with [demo/verify.sql](../demo/verify.sql): **11 invariant checks** (generated from [internal/invariants/checks.go](../internal/invariants/checks.go), the same checks the worker exports as live gauges) that must all return 0 rows.

## Load tests and chaos

Everything runs in Docker (k6 included; nothing to install) against `make up-bench`: the normal settings, plus a mock gateway that takes a realistic 50–150 ms per charge.

```bash
make up-bench && make seed-load          # stack + load data (1 rush show, 1 smoke show, 200 shows x 200 seats)

RUNS=3 ./loadtest/run.sh rush            # 5,000 booking attempts on 100 seats in 10 s -> exactly 100 bookings
RUNS=3 ./loadtest/capacity.sh            # step the arrival rate up and report the knee
RUNS=3 ./loadtest/run.sh spike           # 10x traffic for 30 s, latency must recover
./loadtest/run.sh soak                   # 10-minute soak: heap, goroutines, pools, backlog must stay flat

./chaos/redis-down.sh                    # Redis killed 30 s under load: holds fail closed, confirms still work
./chaos/worker-kill.sh                   # worker SIGKILLed: the PENDING backlog drains exactly once
./chaos/slow-gateway.sh                  # 3 s gateway: confirms slow down, holds don't
./chaos/postgres-restart.sh              # 503s (never 500s), /readyz flips, self-recovery
make up-scale && ./chaos/api-replica.sh  # 1 of 2 replicas behind nginx killed: same-key retry, 0 double charges
```

- **Open-model load** (arrival rates), so a slow server can't hide behind fewer requests ([ADR-0008](adr/0008-open-model-load-testing.md)).
- **409 is a correct answer**: responses are classified as expected, shed (`503 SERVICE_BUSY`) or unexpected, and only unexpected ones fail a run.
- **After every run the script checks correctness**, not just speed: time to consistency, exact booking counts, gateway charges == bookings + refunds, `verify.sql` 0 rows, fresh invariant gauges at 0.
- Each run records its environment (`loadtest/env.sh`) and k6's own CPU and memory.
- pprof runs on the internal metrics ports (`:9091` / `:9092` `/debug/pprof`) for profiling under load.
