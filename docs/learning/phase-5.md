# Phase 5 study notes: observability

Goal of Phase 5: make SeatLock **visible**. Phases 1–4 made it correct; Phase 5 makes it possible to *prove* that while it runs, and to know within seconds when something goes wrong. SeatLock has no frontend, so **the Grafana dashboard is its UI**.

```
api :9091/metrics ─┐
worker :9092/metrics ─┼──> Prometheus (scrapes every 5s, evaluates alerts.yml) ──> Grafana "SeatLock — Reliability"
payment :8090/metrics ─┘
```

---

## 1. Counters, gauges and histograms (with SeatLock examples)

| Type | What it is | Only goes… | SeatLock example | Typical query |
|---|---|---|---|---|
| **Counter** | a running total of events | up (resets to 0 on restart) | `seatlock_holds_total{outcome="seat_held"}`: holds that lost a race | `rate(…[1m])`: events per second |
| **Gauge** | a current value | up and down | `seatlock_invariant_violations{check="double_booking"}`, `seatlock_db_pool_connections{state="acquired"}` | the value itself, `max(…)` |
| **Histogram** | counts of observations in buckets (≤5ms, ≤10ms, … ≤5s) | up | `seatlock_http_request_duration_seconds` | `histogram_quantile(0.95, …)`: p95 |

Rule of thumb: **count things that happen** (requests, outcomes, retries), **gauge things that are** (backlog, pool size, violations right now), and **histogram things that take time** (latency).

You never read a counter's raw value: it depends on when the process started. You read its **rate**. That's why a restart (counter back to 0) doesn't break any panel: `rate()` handles resets.

---

## 2. Why there are no user IDs in labels (cardinality)

Every distinct **combination of label values** is a separate time series, and Prometheus keeps the recent samples of every active series **in memory**.

| Labels on `http_requests_total` | Series |
|---|---|
| `route` (8 patterns) × `method` (3) × `status` (6) | **144**: fine |
| + `user_id` (100,000 users) | **14.4 million**: Prometheus falls over |
| raw path `/v1/holds/123` instead of the pattern | one series per hold **forever**: an ever-growing leak |

So SeatLock's rules ([internal/metrics/metrics.go](../../internal/metrics/metrics.go)):
- `route` is the **chi route pattern** read *after* routing (`/v1/holds/{holdID}`). Unmatched URLs become `unmatched`, so a bot scanning random paths can't create series.
- `method` outside GET/POST/… becomes `OTHER`: the client chooses the method string, so it must be bounded too.
- Outcome labels come from **fixed lists in code** (`created`, `seat_held`, …), never from input.
- **No label is called `job` or `instance`.** Prometheus adds those itself (the scrape target). A metric with its own `job` label gets renamed to `exported_job`, which silently breaks every query joining on it. Worker jobs therefore use `worker_job`.

A unit test gathers every metric and fails on reserved labels or id-like labels ([metrics_test.go](../../internal/metrics/metrics_test.go)). Per-entity detail (which user, which hold) belongs in **logs**, which are built for high cardinality; metrics are for aggregates.

---

## 3. RED for the API, and how the worker fits

**RED** = **R**ate, **E**rrors, **D**uration: the three questions to ask of any request-serving system.

| | SeatLock metric | Dashboard |
|---|---|---|
| Rate | `rate(seatlock_http_requests_total[1m])` by route | "Requests per second by route and status" |
| Errors | the same, `status=~"5.."` | same panel; `HighErrorRate` alert |
| Duration | `seatlock_http_request_duration_seconds` | "Latency p50/p95/p99"; `SlowHolds` alert |

The **worker** isn't request-driven, so RED becomes "runs, failures, duration, **and freshness**":
- `seatlock_worker_job_runs_total{worker_job,result}`: rate and errors;
- `seatlock_worker_job_duration_seconds`: duration;
- `seatlock_worker_rows_total{worker_job,outcome}`: what it actually did (booked, refunded, abandoned, retried);
- `seatlock_worker_last_success_timestamp_seconds`: **freshness**. A background job's typical failure isn't "slow", it's "**silently stopped**". `WorkerJobStalled` compares time-since-success with 5× the job's own interval (`seatlock_worker_job_interval_seconds`), so one rule covers every job.

Domain metrics sit on top: holds and confirms by outcome, idempotency results, gateway calls, Redis lock ops. **RED tells you *that* something is wrong; domain metrics tell you *what*.** A 503 spike plus `holds{outcome="lock_unavailable"}` means "Redis is down and we're failing closed", not "the API is broken".

---

## 4. Invariants as metrics, and "alert on symptoms users feel"

`demo/verify.sql` has 11 queries that must always return 0 rows. Until Phase 5, someone had to *run* them. Now the worker's `invariants` job runs every check every 15s and exports `seatlock_invariant_violations{check}`.

**Why this is stronger than logging:**
- A log line saying "double booking!" is only as good as the person or tool reading it. A gauge is **evaluated continuously** by an alert rule: `DoubleBookingDetected` fires within seconds of the row appearing.
- It checks the **result**, not the machinery. Even if a bug slips past every unit test, a double booking still shows up as a row, and the gauge sees it. It's an independent second opinion on correctness.
- It's an **end-to-end symptom** users actually feel: someone paid without a seat, two people own the same seat. Alerting on symptoms (what users experience) beats alerting on every possible cause: causes are endless, and symptoms are few and meaningful. That's why SeatLock alerts on double bookings, stuck payments, 5xx rate and slow holds, plus only the few causes that need a human (dead letters, a stalled worker, checks not running).

**Single source of truth:** the checks are defined once in [internal/invariants/checks.go](../../internal/invariants/checks.go). `demo/verify.sql` is **generated** from them (`make verify-sql`), and a test fails if the two differ. The demos and the alert run the same SQL.

**Never a false green:** if a check's query fails or hits the `statement_timeout` (2s), its gauge is **not** set to 0. It keeps its last value, `seatlock_invariant_check_errors_total{check}` increases, and `seatlock_invariant_last_success_timestamp_seconds` stops moving, so `InvariantChecksNotRunning` fires after 2 minutes. "Could not check" must never look like "healthy". Before the first successful run there is **no** gauge at all ("no data"), not a reassuring 0.

**Cost:** 11 `COUNT(*)` queries plus 5 small aggregates every 15s, one at a time, each capped at 2s. Most use partial indexes; the join checks scan tables of a few thousand rows (milliseconds). At much larger scale the heavy checks would move to a replica or run less often.

---

## 5. Reading p95/p99, and why averages lie

**p95 = 250ms** means 95% of requests were faster than 250ms, and **5% were slower**.

Why not the average? Say 99 requests take 10ms and 1 takes 5s:
- average = (99 × 10 + 5000) / 100 = **59.9ms**, which looks fine;
- p99 = **5s**, so 1 user in 100 waited five seconds.

The average blends the slow tail into the fast majority and hides it. Users don't experience averages; they experience *their* request. At scale, "1% of requests" is thousands of people, and a single page load makes several requests, so tail latency hits nearly everyone eventually.

How Prometheus computes it: `histogram_quantile(0.95, sum by (le) (rate(…_bucket[5m])))` estimates the 95th percentile from bucket counts. It's an **estimate** within the bucket boundaries (5, 10, 25, 50, 100, 250ms, …), which is why the buckets sit densest where healthy latencies live.

---

## 6. Every alert: what fires it, what it means, first thing to check

| Alert | Fires when | Means | First thing to check |
|---|---|---|---|
| **DoubleBookingDetected** (critical) | any `seatlock_invariant_violations{check}` > 0 | Seats or money are wrong **right now** | Which `check`; run `demo/verify.sql` to see the rows; then the logs around those ids |
| **InvariantChecksNotRunning** (critical) | no successful check for > 2 min | We're **blind** to correctness | Is the worker up? `seatlock_invariant_check_errors_total` and worker logs ("invariant check failed to run") |
| **PaymentsStuckPending** (critical) | oldest `PENDING` payment > 15 min | Someone may have paid with no seat and no refund | `payment_reconciler` runs/errors; gateway reachability (`no_answer` in gateway results) |
| **RedisLockUnavailable** (critical) | `holds{outcome="lock_unavailable"}` rate > 0 for 1 min | **Fail-closed in action**: nobody can start a booking (confirms still work) | `docker compose ps redis`; `redis_lock_ops{result="error"}`; Redis pool timeouts |
| **OutboxDeadLetters** (warning) | any outbox row `FAILED` | A refund or lock release gave up after 20 tries; a refund may be owed | `SELECT id, kind, last_error FROM outbox WHERE status='FAILED'`; fix, then `make outbox-retry ID=…` |
| **WorkerJobStalled** (warning) | time since a job's last success > 5× its interval | That job's work piles up (refunds, expiries, reconciliation) | Worker logs for "job run failed"; DB reachability from the worker |
| **HighErrorRate** (warning) | 5xx > 1% over 5 min | Users see failures | RPS-by-status panel: 503 `LOCK_UNAVAILABLE` (Redis), 503 `SERVICE_BUSY` (DB), 504 (gateway) or 500 (a bug: logs) |
| **SlowHolds** (warning) | p95 of `POST /v1/holds` > 250ms for 5 min | The hottest path of a sale is slow | DB pool acquire wait; Redis lock latency; database load |
| **DBPoolSaturated** (warning) | acquire wait > 0.1 s/s over 5 min | Requests queue for DB connections, which eats their timeouts | `DB_MAX_CONNS`; slow queries; long transactions |

All rules are unit-tested with `promtool test rules` ([monitoring/alerts_test.yml](../../monitoring/alerts_test.yml)): for example, `WorkerJobStalled` fires for a job stuck at t=0 and stays silent for one whose last success keeps up with the clock. promtool also caught a real PromQL bug while this was being written: `5 * on(worker_job) …` puts vector matching on a scalar multiplication. The match belongs on the comparison.

---

## 7. The Phase 4 review fix (step 0): a stale lock release

The Redis lock's value is the **user**, not the hold. That allowed this sequence:

```
1. user A's hold expires; the expirer writes RELEASE_LOCK(owner=A)
2. before the drainer runs, A holds the SAME seat again
   (Redis: same owner -> TTL refreshed; Postgres: a new ACTIVE hold)
3. the drainer runs the stale row: "owner == A" -> it deletes A's CURRENT lock
```

That's still safe, because Postgres rejects anyone else, but Redis would say "free" while Postgres holds the seat. That breaks the "Redis outlives the DB hold" rule and pushes contention back onto Postgres.

**Fix:** before releasing, the drainer asks Postgres, "does this user have a live (ACTIVE, not dead) hold on this seat right now?", using the same `deadHoldCondition` rule as everywhere else. If yes, the row is stale: skip the release and mark it DONE. Test: `TestDrainer_StaleReleaseLock_KeepsSameUsersNewLock`.

**Known limitation / future:** a tiny window remains, if the user re-holds *between* that check and the release. The proper fix is a **hold-scoped lock token**: store the hold id (or a random token per hold) as the Redis value instead of the user id, so a release can only ever delete the lock of the hold it belongs to.

---

## 8. Interview questions (with short answers)

**Q1. How would you know in production that a double booking happened?**
Not from a customer complaint. The worker runs the correctness queries every 15s and exports `seatlock_invariant_violations{check="double_booking"}`. `DoubleBookingDetected` fires on any value above 0 within seconds, and the dashboard's top row turns red. And if the checks themselves stop running, `InvariantChecksNotRunning` fires, so "no data" can't hide a problem.

**Q2. What's the difference between p95 and average latency?**
The average blends everything together and hides the slow tail. p95 says "95% were faster than this, 5% were slower". 99 requests at 10ms plus one at 5s average 60ms, but the p99 is 5s. Users feel their own request, so alert on percentiles.

**Q3. Why not put `user_id` as a metric label?**
Every label combination is a separate in-memory series: 100k users × 144 base series = 14.4M series, which takes Prometheus down. Per-user detail belongs in logs; metrics hold bounded aggregates.

**Q4. Counter, gauge or histogram for: requests, queue length, latency?**
A counter for requests (read with `rate`), a gauge for queue length (a current value that goes up and down), and a histogram for latency (to compute percentiles).

**Q5. Why serve `/metrics` on a separate port?**
Metrics reveal internals (traffic, error rates, pool sizes) that shouldn't be public. A separate internal port is never exposed by the load balancer, and scraping keeps working even when the public port is saturated or rate-limited.

**Q6. What is RED, and how do you apply it to a background worker?**
Rate, Errors, Duration for request-driven services. For a worker, add **freshness**: a job's last successful run compared with its expected interval. Background jobs usually fail by silently stopping, not by getting slow.

**Q7. What should you alert on?**
Symptoms users feel or money at risk (double bookings, stuck payments, error rate, slow holds), plus the few causes that need a human (dead letters, stalled worker, checks not running). Not every cause: too many alerts train people to ignore them.

**Q8. Your invariant query times out. What does the dashboard show?**
The last real value, not 0. The error counter increases and last-success stops moving; after 2 minutes `InvariantChecksNotRunning` fires. Showing 0 would be a "false green", the most dangerous thing a correctness monitor can do.

**Q9. Why is "Redis lock unavailable" a critical alert if the system handled it correctly?**
Because handling it correctly means **refusing** every new booking (fail-closed). Correct and acceptable for a few seconds, but a total outage of sales for users. The metric proves we failed closed; the alert gets a human to bring Redis back.

**Q10. How do you know your alert rules work before an incident?**
Unit-test them: `promtool test rules` feeds synthetic series and asserts which alerts fire when. SeatLock tests `DoubleBookingDetected`, `WorkerJobStalled` (stalled vs healthy) and `OutboxDeadLetters`. Plus an integration test that inserts a real violation and checks the gauge becomes 1.

**Bonus. How do you stop the dashboard and the SQL checks from drifting apart?**
One source: the checks live in Go, `verify.sql` is generated from them, and a test fails if the committed file differs.

---

## 9. What changes in Phase 6

Phase 6 puts **real load** on this dashboard. **k6** scripts simulate a flash sale (thousands of users racing for 100 seats, retries with the same `Idempotency-Key`, payment timeouts), and **chaos** steps kill Redis, slow the gateway, or kill the worker mid-sale. The dashboard then shows real numbers: requests per second, p95/p99 under load, how fast holds fail closed and recover, how quickly the worker drains the backlog. Through all of it, the Correctness row must stay **green**: zero double bookings, every payment either booked or refunded. Those numbers, and that green row, become the project's headline results.
