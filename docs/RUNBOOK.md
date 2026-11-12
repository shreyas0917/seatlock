# SeatLock runbook

What to do when an alert fires, how to operate the stack, and example SLOs. Every alert in [monitoring/alerts.yml](../monitoring/alerts.yml) links here through its `runbook_url` annotation.

Written for the local Docker Compose stack; in production the same checks apply with your own log, metrics and database access.

## Contents
- [Shortcuts used below](#shortcuts-used-below)
- Alerts (critical): [DoubleBookingDetected](#doublebookingdetected) · [InvariantChecksNotRunning](#invariantchecksnotrunning) · [PaymentsStuckPending](#paymentsstuckpending) · [RedisLockUnavailable](#redislockunavailable)
- Alerts (warning): [OutboxDeadLetters](#outboxdeadletters) · [WorkerJobStalled](#workerjobstalled) · [HighErrorRate](#higherrorrate) · [SlowHolds](#slowholds) · [DBPoolSaturated](#dbpoolsaturated)
- [Common operations](#common-operations)
- [Example SLOs](#example-slos)

## Shortcuts used below

```bash
# SQL against the app database
psql_seatlock() { docker compose exec -T postgres psql -U seatlock -d seatlock -c "$1"; }
# An instant Prometheus query (or paste the expression into http://localhost:9095/graph)
promq() { curl -s -G http://localhost:9095/api/v1/query --data-urlencode "query=$1"; echo; }
```

Dashboards: **Grafana** http://localhost:3001 ("SeatLock — Reliability", home page) · **Prometheus alerts** http://localhost:9095/alerts.

---

## DoubleBookingDetected

**Severity:** critical, fires immediately. **Expression:** `max by (check) (seatlock_invariant_violations) > 0`. Despite its name it covers **all 11 invariant checks**: the `check` label says which one.

**What it means:** a correctness rule is broken *right now*: a seat booked twice, a booking without exactly one successful payment, money taken without a seat or a refund, a refunded payment whose booking still exists, and so on.

**User impact:** potentially two people with one seat, or a customer charged with nothing to show for it. Treat it as an incident.

**First 3 checks:**
1. Which check, and which rows:
   ```bash
   promq 'seatlock_invariant_violations > 0'
   docker compose exec -T postgres psql -U seatlock -d seatlock < demo/verify.sql
   ```
2. The logs around the ids from step 1 (holds, payments, bookings):
   ```bash
   docker compose logs --since 30m api worker | grep -E '"(hold_id|payment_id|booking_id)":<id>'
   ```
3. The payment story of the affected hold:
   ```bash
   psql_seatlock "SELECT id, status, charge_key, provider_charge_id, created_at, updated_at, last_error FROM payments WHERE hold_id = <hold_id> ORDER BY id"
   psql_seatlock "SELECT id, kind, status, attempts, last_error FROM outbox WHERE payload->>'payment_id' = '<payment_id>'"
   ```

**Recovery:** fix the data by hand only after understanding how it happened (refund through the gateway, then correct the rows). The alert clears on the next invariant run (≤ 15 s) once `verify.sql` returns 0 rows for that check.

## InvariantChecksNotRunning

**Severity:** critical, after 1 min. **Fires when** no invariant check has succeeded for over 2 minutes, or no worker reports them at all.

**What it means:** we are **blind** to correctness. The gauges keep their last value (never a false 0), so the dashboard may look green while nothing is being checked.

**User impact:** none directly, but a real violation would go unnoticed.

**First 3 checks:**
1. Is the worker running, and is it scraped?
   ```bash
   docker compose ps worker
   promq 'up{job="worker"}'
   ```
2. Are the checks failing (timeouts, connection errors)?
   ```bash
   promq 'increase(seatlock_invariant_check_errors_total[5m]) > 0'
   docker compose logs --since 10m worker | grep -i "invariant check failed"
   ```
3. Is Postgres slow? (each check has a 2 s `statement_timeout`)
   ```bash
   psql_seatlock "SELECT pid, state, now() - query_start AS running, left(query, 80) FROM pg_stat_activity WHERE state <> 'idle' ORDER BY running DESC LIMIT 10"
   ```

**Recovery:** `docker compose restart worker` if it is stuck; the alert clears once `min(seatlock_invariant_last_success_timestamp_seconds)` is fresh again.

## PaymentsStuckPending

**Severity:** critical, immediately. **Fires when** the oldest `PENDING` payment is older than 15 minutes.

**What it means:** someone may have been charged with neither a booking nor a refund. Normally the reconciler resolves a `PENDING` payment within `RECONCILE_AFTER` (2 min) + one run.

**User impact:** a customer waiting on "payment pending", possibly charged; their seat stays blocked (up to `PENDING_HOLD_MAX`).

**First 3 checks:**
1. Which payments, and why they are stuck:
   ```bash
   psql_seatlock "SELECT id, hold_id, status, created_at, reconcile_attempts, next_check_at, last_error FROM payments WHERE status = 'PENDING' ORDER BY created_at LIMIT 20"
   ```
2. Is the reconciler running and succeeding?
   ```bash
   promq 'time() - seatlock_worker_last_success_timestamp_seconds{worker_job="payment_reconciler"}'
   docker compose logs --since 15m worker | grep payment_reconciler | tail -20
   ```
3. Can the worker reach the gateway? (`last_error` above usually says)
   ```bash
   promq 'sum by (op, result) (rate(seatlock_payment_gateway_requests_total[5m]))'
   docker compose exec -T worker wget -q -O - http://payment:8090/healthz
   ```

**Recovery:** once the gateway answers, the reconciler books, refunds, declines or (after voiding the key) abandons each payment by itself. Confirm with check 1 returning no old rows.

## RedisLockUnavailable

**Severity:** critical, after 1 min. **Fires when** holds are being refused with `lock_unavailable` (rate > 0 for 1 minute).

**What it means:** Redis can't be reached, so new holds **fail closed** (`503 LOCK_UNAVAILABLE`). This is the designed behaviour ([ADR-0002](adr/0002-fail-closed-when-redis-is-down.md)): correct, but no one can start a booking. Confirms of existing holds keep working.

**User impact:** no new seat holds (a sales outage); existing holds can still pay.

**First 3 checks:**
1. Is Redis up and answering?
   ```bash
   docker compose ps redis
   docker compose exec -T redis redis-cli ping
   ```
2. What the api sees (errors vs timeouts, pool exhaustion):
   ```bash
   promq 'sum by (op, result) (rate(seatlock_redis_lock_ops_total[1m]))'
   promq 'seatlock_redis_pool_connections{job="api"}'
   docker compose logs --since 5m api | grep -i "seat lock unavailable" | tail -5
   ```
3. Readiness (Redis down alone does **not** make the api unready):
   ```bash
   curl -s localhost:8080/readyz
   ```

**Recovery:** `docker compose start redis` (or fix the network). Holds recover by themselves within seconds (measured: 1 s after Redis returned). The alert resolves about a minute later, when the 1-minute rate drops to 0. Expect short `409 SEAT_HELD` answers on seats whose locks were lost: Postgres still decides.

## OutboxDeadLetters

**Severity:** warning, immediately. **Fires when** any outbox row is `FAILED` (gave up after `OUTBOX_MAX_ATTEMPTS` = 20 attempts).

**What it means:** a side effect was not done: usually a **refund owed** to a customer, or a Redis lock release.

**User impact:** a customer may be waiting for their money back.

**First 3 checks:**
1. Which rows, and the last error:
   ```bash
   psql_seatlock "SELECT id, kind, payload, attempts, last_error, updated_at FROM outbox WHERE status = 'FAILED' ORDER BY id"
   ```
2. For a `REFUND`: the payment it belongs to and its state at the gateway:
   ```bash
   psql_seatlock "SELECT id, status, charge_key, provider_charge_id FROM payments WHERE id = <payload.payment_id>"
   curl -s localhost:8095/charges/<charge_key>
   ```
3. The drainer's recent errors:
   ```bash
   docker compose logs --since 30m worker | grep outbox_drainer | grep -i error | tail -10
   ```

**Recovery:** fix the cause (gateway, credentials, data), then re-queue the row: `make outbox-retry ID=<id>`. Refunds are idempotent, so a retry can't refund twice. The alert clears when no row is `FAILED`.

## WorkerJobStalled

**Severity:** warning, immediately. **Fires when** a worker job's last success is older than 5× its own interval (the `worker_job` label says which).

**What it means:** that job's work is piling up (refunds, expiries, reconciliations, invariant checks, key cleanup).

**User impact:** depends on the job: unrefunded money, seats held by expired holds, stuck payments.

**First 3 checks:**
1. Which job, and how long ago it last succeeded:
   ```bash
   promq 'time() - seatlock_worker_last_success_timestamp_seconds'
   ```
2. Its failures:
   ```bash
   promq 'sum by (worker_job, result) (increase(seatlock_worker_job_runs_total[10m]))'
   docker compose logs --since 15m worker | grep -i "job run failed" | tail -10
   ```
3. Database reachability from the worker:
   ```bash
   docker compose exec -T worker wget -q -O - http://localhost:9092/metrics | grep -c seatlock_db_pool
   psql_seatlock "SELECT 1"
   ```

**Recovery:** `docker compose restart worker` if it is wedged; claims are lease-based and every side effect is idempotent, so a restart is always safe. The alert clears after the job's next success.

## HighErrorRate

**Severity:** warning, after 5 min. **Fires when** more than 1% of requests are 5xx over 5 minutes.

**What it means:** users see failures. 409s are not errors and are not counted.

**First 3 checks:**
1. Which route and which status:
   ```bash
   promq 'sum by (route, status) (rate(seatlock_http_requests_total{status=~"5.."}[5m]))'
   ```
2. Decode the status:
   - `503` on holds: `LOCK_UNAVAILABLE` (Redis, see [RedisLockUnavailable](#redislockunavailable)) or `SERVICE_BUSY` (DB, see [DBPoolSaturated](#dbpoolsaturated));
   - `504` on bookings: gateway slow or unreachable (payments stay `PENDING`, reconciled later);
   - `500`: a bug; the logs have the error.
3. The errors themselves:
   ```bash
   docker compose logs --since 10m api | grep '"level":"ERROR"' | tail -20
   ```

**Recovery:** fix the dependency or the bug; the 5-minute rate falls below 1% a few minutes later.

## SlowHolds

**Severity:** warning, after 5 min. **Fires when** p95 of `POST /v1/holds` is above 250 ms over 5 minutes (the same line as the knee rule in [RESULTS.md](benchmarks/RESULTS.md)).

**What it means:** the hottest path of a sale is slow; users wait for a seat.

**First 3 checks:**
1. Is it the database (pool wait)?
   ```bash
   promq 'sum(rate(seatlock_db_pool_acquire_wait_seconds_total{job="api"}[5m]))'
   promq 'seatlock_db_pool_connections{job="api"}'
   ```
2. Is it Redis?
   ```bash
   promq 'sum by (op, result) (rate(seatlock_redis_lock_ops_total[5m]))'
   ```
3. Is it load? (compare with the measured knee: ~514 req/s on one laptop)
   ```bash
   promq 'sum(rate(seatlock_http_requests_total[1m]))'
   docker stats --no-stream
   ```

**Recovery:** remove the load or the slow dependency. Profiling: `docker compose exec -T api wget -q -O - 'http://localhost:9091/debug/pprof/profile?seconds=30' > cpu.pprof` then `go tool pprof -top cpu.pprof`.

## DBPoolSaturated

**Severity:** warning, after 5 min. **Fires when** requests spend more than 0.1 s per second waiting for a Postgres connection.

**What it means:** the pool (`DB_MAX_CONNS`, 25) is full and requests queue for it, eating into their 3 s timeout. Past it they get `503 SERVICE_BUSY`.

**First 3 checks:**
1. Pool state and acquires:
   ```bash
   promq 'seatlock_db_pool_connections{job="api"}'
   promq 'sum(rate(seatlock_db_pool_acquires_total{job="api"}[5m]))'
   ```
2. Slow or blocked queries:
   ```bash
   psql_seatlock "SELECT pid, wait_event_type, wait_event, now() - query_start AS running, left(query, 80) FROM pg_stat_activity WHERE state <> 'idle' ORDER BY running DESC LIMIT 10"
   ```
3. Postgres CPU:
   ```bash
   docker stats --no-stream seatlock-postgres-1
   ```

**Recovery:** reduce load or slow queries. Raise `DB_MAX_CONNS` only if Postgres has spare CPU (Little's law: connections needed ≈ acquires per second × seconds each is held; see [phase-6.md §5](learning/phase-6.md)).

---

## Common operations

**Restart a component safely**
```bash
docker compose restart worker      # always safe: lease-based claims, idempotent side effects
docker compose restart api         # in-flight confirms may end 504; their payments are reconciled
docker compose restart redis       # holds fail closed (503) for a few seconds; confirms keep working
docker compose restart postgres    # api answers 503 (never 500) for a few seconds and recovers by itself
```

**Drain the outbox / see the backlog**
```bash
psql_seatlock "SELECT kind, status, COUNT(*), MIN(next_attempt_at) FROM outbox GROUP BY kind, status"
```
The drainer runs every 5 s; a backlog that doesn't shrink means the drainer is failing (see [WorkerJobStalled](#workerjobstalled)).

**Re-queue a dead letter** (after fixing its cause)
```bash
make outbox-retry ID=<id>
```

**Reset demo data** (frees every seat in Postgres and Redis, keeps shows and seats)
```bash
make reset-data
curl -s -X POST localhost:8095/debug/reset    # also forget the mock gateway's charges
```

**Read `verify.sql` output**
```bash
docker compose exec -T postgres psql -U seatlock -d seatlock < demo/verify.sql
```
Each `CHECK …` header is followed by the violating rows. **Healthy = every check prints `(0 rows)`.** Any row is an incident (see [DoubleBookingDetected](#doublebookingdetected)). The same 11 checks run every 15 s in the worker as `seatlock_invariant_violations{check}`.

---

## Example SLOs

> **Example targets for this project**, to show how SLOs would be set and measured with the metrics SeatLock already has. They are **not** measured production SLOs: the only measurements are the single-laptop runs in [RESULTS.md](benchmarks/RESULTS.md).

| SLO | Target (example) | Measured with | Error budget (30 days) |
|---|---|---|---|
| **Hold availability**: holds answered with a correct answer (201/200/409), excluding client errors | 99.9% | `sum(rate(seatlock_http_requests_total{route="/v1/holds",status!~"5.."}[30d])) / sum(rate(seatlock_http_requests_total{route="/v1/holds"}[30d]))` | 0.1% of hold requests may be 5xx: ~43 minutes of total outage, or spread across a month |
| **Hold latency**: p95 of `POST /v1/holds` under 250 ms | 99% of 5-minute windows | `histogram_quantile(0.95, sum by (le) (rate(seatlock_http_request_duration_seconds_bucket{route="/v1/holds"}[5m]))) < 0.25` (the `SlowHolds` alert is the burn signal) | ~7 hours of slow windows per month |
| **Correctness**: 0 invariant violations | **100%: a hard guarantee, not a budget** | `max(seatlock_invariant_violations) == 0` and `InvariantChecksNotRunning` not firing | **None.** Any violation is an incident, not budget spend |

How the budget would be used: while hold availability is within budget, ship freely; when a month's budget is mostly spent (for example by Redis outages, where fail-closed answers 503 by design), prioritise reliability work: a Redis replica, or an admission limit that sheds earlier. `LOCK_UNAVAILABLE` 503s count against availability on purpose: refusing safely is still an outage for the user.
