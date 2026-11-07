# SeatLock: load test and chaos results

**These are single-machine, local numbers from one laptop. They are not production capacity.**
Everything ran on one machine at the same time: SeatLock, Postgres, Redis, the mock gateway, Prometheus, Grafana, **and the load generator (k6)**. They all shared the same 8 CPU threads, so the numbers are a lower bound for SeatLock on that hardware. They also say nothing about a real cluster. Read [what these numbers do and don't mean](#what-these-numbers-do-and-dont-mean) before quoting any of them.

Every number below is the **median of 3 runs (min–max)**, unless it says otherwise. The raw k6 summaries of the final runs are in [runs/](runs/).

## Contents
- [Environment](#environment)
- [Rules these numbers follow](#rules-these-numbers-follow)
- [How to reproduce](#how-to-reproduce)
- [1. Rush: 5,000 booking attempts on 100 seats](#1-rush-5000-booking-attempts-on-100-seats)
- [2. Capacity: finding the knee](#2-capacity-finding-the-knee)
- [3. Spike: 10× traffic for 30 s](#3-spike-10-traffic-for-30-s)
- [4. 10-minute soak](#4-10-minute-soak)
- [5. Chaos under load](#5-chaos-under-load)
- [6. The bottleneck: measure → change → measure](#6-the-bottleneck-measure--change--measure)
- [Grafana screenshots to take](#grafana-screenshots-to-take)
- [What these numbers do and don't mean](#what-these-numbers-do-and-dont-mean)

## Environment

`loadtest/env.sh` records this for every run (`env.txt` next to each run's results).

| | |
|---|---|
| CPU | Intel Core i5-1135G7 @ 2.40 GHz: 4 cores / 8 threads (a laptop CPU; it can throttle under sustained load) |
| RAM | 19.7 GiB host; **Docker Desktop VM: 8 CPUs, 7.76 GiB** |
| OS | Windows 11 Pro 10.0.22621, Docker Desktop (server 29.6.1) |
| Versions | Go 1.26 (`golang:1.26-alpine`), PostgreSQL 16.15, Redis 7.4.11, k6 0.54.0 (`grafana/k6:0.54.0`) |
| SeatLock | **1 api replica**, 1 worker; `DB_MAX_CONNS=25`, `DB_MIN_CONNS=5`, `REDIS_POOL_SIZE=10`, Redis timeouts 200 ms, `HOLD_TTL=300s`, `PAYMENT_TIMEOUT=5s`, `DB_QUERY_TIMEOUT=3s`; Postgres `max_connections=100` |
| Mock gateway | **50–150 ms** random delay per charge (`make up-bench`, a realistic payment API, not zero) |
| Profiling | pprof block/mutex profiles **off** for every reported run (on only for the profiling session in §6) |
| Shared CPUs | k6, Prometheus (5 s scrapes) and Grafana ran on the same 8 threads as the system under test. Each run also records k6's own CPU and memory (`docker stats`): about 75–140% CPU and ~220 MiB at the knee |
| Other load | Unrelated containers on the machine were **paused** for every benchmark run |
| Load data | `make seed-load`: 1 rush show (100 seats), 1 smoke show (20 seats), 200 shows × 200 seats |

## Rules these numbers follow

- **Open model** (k6 `constant-arrival-rate` / `ramping-arrival-rate`). New attempts start on schedule whether or not earlier ones have finished, like real users. A closed model (N users in a loop) sends *less* traffic as the server slows down, which hides the slowdown. That bias is called *coordinated omission*. Each run also reports `dropped_iterations`, the attempts k6 could not start, so a run that didn't really deliver its rate can't pass as one that did.
- **409 is a correct answer, not an error.** `409 SEAT_HELD` / `SEAT_ALREADY_BOOKED` means "someone else got this seat". `loadtest/lib.js` sorts every response into three classes:
  - **expected**: 200, 201, and 409 with a seat-conflict code;
  - **shed**: `503 SERVICE_BUSY`, the API refusing work it can't finish in time. Only the capacity test accepts it;
  - **unexpected**: everything else, including any 500 or a 503 where none should happen.
  
  `http.setResponseCallback(http.expectedStatuses(...))` keeps k6's own `http_req_failed` consistent with that.
- **Correctness is checked after every run, not assumed** (`loadtest/k6lib.sh correctness_checks`):
  1. Wait until 0 PENDING payments and 0 PENDING outbox rows (**time to consistency**).
  2. Bookings in Postgres == bookings k6 saw, plus at most the confirms answered `504 PAYMENT_PENDING` (their outcome is resolved later).
  3. Gateway: charges that took money == bookings + refunds, and at most 1 charge and 1 refund per key.
  4. `verify.sql`: all 11 checks return 0 rows.
  5. A *fresh* Prometheus invariant run reports all 11 gauges at 0.
- **Reset and warm up before every run:** free all seats, reset the gateway, run 15 s of light mixed traffic, reset again.
- **3 runs each, median reported**, except where noted (§2 single steps, the soak).
- **k6 runs inside the Compose network** and targets `http://api:8080`, not `localhost:8080`. On Windows, Docker Desktop's host port proxy adds latency of its own, and going through it would measure the proxy.

## How to reproduce

```bash
make up-bench            # the stack with the 50-150 ms gateway delay
make seed-load           # load data (idempotent)

RUNS=3 ./loadtest/run.sh rush                                            # §1
RUNS=3 CAP_STEPS="400 500 600 700 800" CAP_STOP_AFTER=9 ./loadtest/capacity.sh  # §2 (3x steps)
RUNS=1 CAP_STEPS="300 1000" CAP_STOP_AFTER=9 ./loadtest/capacity.sh     # §2 (1x steps)
RUNS=3 K6_ENV="SPIKE_BASE=60" ./loadtest/run.sh spike                     # §3
K6_ENV="SOAK_RATE=300 SOAK_DURATION=10m" ./loadtest/run.sh soak          # §4
CHAOS_RATE=300 ./chaos/redis-down.sh     # §5, likewise worker-kill, slow-gateway, postgres-restart
make up-scale && CHAOS_RATE=300 ./chaos/api-replica.sh
PAIRS=3 RATE=800 ./loadtest/ab.sh seatlock-api:before seatlock-api:after # §6
```

Every script prints PASS/FAIL per check and exits non-zero on any failure. Results land in `tmp/loadtest/` and `tmp/chaos/`.

## 1. Rush: 5,000 booking attempts on 100 seats

**Model:** `constant-arrival-rate`, **500 booking attempts/s for 10 s** (k6 started **5,001** attempts in each run: the arrival-rate executor also counts the one at t = 10 s) on one show with 100 seats. 70% of attempts want one of the 20 "popular" seats; 30% pick any seat. Each attempt is a new user: hold the seat, and on `201` pay with its own `Idempotency-Key`. **Wording:** "5,000 booking attempts arriving in a 10-second burst". It is *not* "5,000 simultaneous users": each attempt finishes in milliseconds, so k6 had at most **13** VUs in flight in the median run (104 in the slowest run).

| | Median (min–max) of 3 runs |
|---|---|
| Bookings | **100** (**exactly 100 in every run**, 3 of 3 PASS) |
| Hold: p50 / p95 / p99 | **3.6 / 15.0 / 44.7 ms** (p95 range 13.4–93.6) |
| Confirm (incl. 50–150 ms gateway): p50 / p95 / p99 | 166.7 / 251.2 / 282.2 ms (p95 range 205.8–637.4) |
| Hold → booked, end to end: p50 / p95 / p99 | 212.0 / 321.3 / 350.1 ms |
| Statuses (every run) | holds: **201 × 100, 409 × 4,901**; confirms: **201 × 100** |
| Unexpected statuses / dropped attempts | **0 / 0** in every run |
| Charges / refunds | **100 / 0** in every run (gateway: charges that took money == bookings + refunds) |
| `verify.sql` / invariant gauges | 0 rows / all 0, in every run |
| Time to consistency | 0–1 s |
| k6's own peak CPU / memory | 116% / 224 MiB (range 103–200%) |

## 2. Capacity: finding the knee

**Model:** the realistic mix (`capacity.js`) across 200 shows × 200 seats, at a fixed arrival rate per step, 60 s per step. 70% of actions read a seat map; 30% hold a random seat, then confirm (half) or cancel (half). By request that's about **54.6% seat maps, 23% holds, 11% confirms and 11% cancels** (measured). RPS counts HTTP requests; one action is 1–2 requests.

**The knee rule:** the highest step where (medians) **p95(hold) < 250 ms AND shed + unexpected < 0.1%**, with k6 delivering the rate (0 dropped). A 500 at any step, even past the knee, counts as a bug.

**Runs:** steps 400–800 ran **3× each (median shown)**; 300 and the step past the knee (1000) ran **1×** to save time. Because 300 (1×) and 500 (3×) failed between steps that passed, both were **re-run 3×** to investigate; both results are shown.

| Target (actions/s) | Runs | Achieved RPS | p50 / p95 / p99 hold (ms) | p95 seat map | p95 confirm | p95 cancel | Shed % | Unexpected % | Dropped | 500s | Knee rule? |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 300 | 1× | 381 | 185.7 / 755.1 / 1013.8 | 373.0 | 1564.5 | 726.4 | 0 | 0 | 342 | 0 | no |
| 300 (re-run) | 3× | 387 | 11.8 / **67.7** / 132.1 | 26.7 | 246.9 | 74.4 | 0 | 0 | 0 | 0 | yes |
| **400** | 3× | **514** | 13.8 / **86.1** / 144.0 | 32.4 | 281.1 | 91.6 | 0 | 0 | 0 | 0 | **yes** |
| 500 | 3× | 641 | 23.4 / 458.6 / 598.7 | 229.7 | 958.1 | 465.3 | 0 | 0 | 117 | 0 | no |
| 500 (re-run) | 3× | 620 | 204.3 / 954.8 / 1287.2 | 514.0 | 1924.6 | 969.2 | 0 | 0 | 1005 | 0 | no |
| 600 | 3× | 768 | 10.7 / 76.5 / 172.2 | 28.8 | 241.8 | 77.5 | 0 | 0 | 0 | 0 | yes |
| 700 | 3× | 821 | 571.1 / 2263.6 / 2576.5 | 1166.3 | 4638.2 | 2239.9 | 0 | 0 | 3515 | 0 | no |
| 800 | 3× | 826 | 1469.7 / 3602.2 / 4025.8 | 1877.6 | 6704.4 | 3492.9 | 0 | 0 | 9204 | 0 | no |
| 1000 (past the knee) | 1× | 849 | 2980.7 / 4732.3 / 5466.3 | 3050.5 | 10483.4 | 4808.1 | **26.2** | 0.371 | 17171 | **0** | no |

All latencies in ms; medians of the runs shown. Every one of these runs (all 29) passed every correctness check: exact bookings, charges == bookings + refunds, `verify.sql` 0 rows, invariants 0. **No step, not even 1000/s, produced a single HTTP 500.** At 1000/s the API sheds 26% of requests with `503 SERVICE_BUSY` (correct overload behaviour); the 0.37% unexpected are `504 PAYMENT_PENDING` confirms, resolved afterwards.

### The knee, stated honestly

- **Conservative knee: 400 actions/s ≈ 514 HTTP requests/s** (median p95 hold 86 ms, 0 errors). It is the highest step where that step **and every step below it** met the rule in its median.
- **By the rule alone, 600 actions/s (≈ 768 RPS, p95 hold 77 ms) also passed**, but **500 failed in all 6 of its runs** (two separate sets of 3) and 300 failed its first single run. A knee above a step that fails 6 out of 6 times would be a lucky number, so **this document uses 400 / 514 RPS**.
- **Why the steps aren't monotonic, and what was ruled out:**
  - The CPU clock, sampled every minute during the re-run, sat at **82–128% of the 2.4 GHz base** (turbo is 4.2 GHz). The laptop throttles under sustained load, so the same step can come out differently depending on when it runs.
  - Postgres checkpoints (every 5 min, 2–13 MB each) don't line up with the slow runs.
  - The 500/s anomaly is **not explained**. A hypothesis, not tested: at 500/s k6 pre-allocates 250 VUs (300 at 600/s). Once latency rises it has to start new VUs mid-run (it dropped 117–1005 iterations), and that costs CPU on an already saturated machine, which keeps it slow.
- **At the knee**, peak CPU per container (median run): Postgres ~200%, api ~167%, k6 ~89%, out of the 8 threads everything shares.

**Correction:** the request split is **≈ 54.6% seat maps, 23% holds, 11% confirms, 11% cancels** (measured: 419.9 of 768.5 RPS were seat maps at 600/s). Commit `9685d95`'s message says "64% of requests"; that was a calculation slip, and this split is the right one.

**What limits it:** see §6. DB round trips and a shared, throttling CPU: past ~500–700 actions/s the whole machine runs out of CPU (Postgres, the api and k6 together).

## 3. Spike: 10× traffic for 30 s

**Model:** `ramping-arrival-rate`, the same mix: **60 actions/s** for 30 s → ramp to **600/s** in 5 s → hold for 30 s → back to 60/s, then 80 s of recovery (measured from t = 90 s). **Strict:** any 503 counts as unexpected.

| Phase | p50 / p95 / p99 hold (ms) | p95 seat map (ms) | Unexpected |
|---|---|---|---|
| Baseline (60/s) | 8.3 / 25.1 / 78.9 | 5.7 | 0 |
| Spike (600/s) | 1,342 / 3,620 / 3,930 | 1,641 | 0 |
| Recovery | 8.5 / 26.6 / 72.3 | 6.5 | 0 |

Medians of 3 runs. Pass rule: recovery p95(hold) ≤ 1.5 × baseline + 5 ms, 0 unexpected, `verify.sql` 0 rows.

**Result: 2 of 3 runs PASS, 1 FAIL**, reported as measured (not re-run until it passed):
- **All 3 runs:** 0 unexpected statuses (no 503, no 500), `verify.sql` 0 rows, charges == bookings + refunds.
- **Runs 1 and 3** recovered fully: recovery p95 hold 26.6 / 22.1 ms against baselines of 37.4 / 25.1 ms.
- **Run 2** recovered only partly within the window: recovery p95 hold **109.7 ms against a 20.7 ms baseline**. The queue built during the spike took longer than the 20 s settling time to drain.

**Why the spike hurts this much:** 600/s is above the conservative knee (400/s), on a throttling CPU (§2). During the spike every request queues (p95 hold 3.6 s), but nothing fails: the API slows down instead of dropping requests. What it doesn't do is **shed early**: requests wait up to `DB_QUERY_TIMEOUT` (3 s) before a 503. An admission limit, or the waiting room, would shed sooner and keep latency low for the requests it accepts.

## 4. 10-minute soak

**Model:** `constant-arrival-rate` at **300 actions/s for 10 minutes** (50% of the rule-knee 600, 75% of the conservative knee 400), 1 run. A soak test looks for slow leaks. The run books 10% of its holds so it doesn't use up the seats. `run.sh` compares Prometheus averages over minutes 2–4 with the last 2 minutes:

| | Minutes 2–4 (avg) | Last 2 minutes (avg) | Allowed | Result |
|---|---|---|---|---|
| api heap in use | 22.7 MiB | 22.7 MiB | ≤ ×1.3 + 8 MiB | flat |
| api goroutines | 172 | 203 | ≤ ×1.3 + 20 | flat (within the rule: in-flight requests vary with latency) |
| worker heap in use | 9.8 MiB | 11.3 MiB | ≤ ×1.3 + 8 MiB | flat |
| worker goroutines | 15.0 | 15.2 | ≤ ×1.3 + 20 | flat |
| api DB connections (open) | 25 | 25 | ≤ +5 | flat (pool size) |
| api Redis connections (open) | 10 | 10 | ≤ +5 | flat (pool size) |
| PENDING outbox rows | 2.4 | 0.9 | ≤ +10 | no backlog growth |
| oldest PENDING payment | 0.1 s | 0.1 s | ≤ +30 s | no stuck payments |

Load: **230,616 requests in 10 minutes (384 RPS)**; p95 seat map 45.7 ms, hold 119.2 ms, confirm 348.3 ms, cancel 127.1 ms. **0 unexpected statuses**, 5,099 bookings, charges == bookings + refunds, `verify.sql` 0 rows, time to consistency 1 s. **1 run, PASS.**

## 5. Chaos under load

Each script runs `capacity.js` at **300 actions/s (≈ 380 RPS) for 150 s**: 50% of the rule-knee (600), 75% of the conservative knee (400), so a harder test than planned. 1 run each, all **5 PASS**, breaks one thing about 30 s in, and prints a Prometheus timeline (10 s steps) of what users saw. Then it runs the same correctness checks as every benchmark. *Time to consistency* is how long after the load stopped until there were 0 PENDING payments and 0 PENDING outbox rows.

| Scenario | What users saw (k6, whole 150 s run) | Alert | Recovery | Time to consistency | Double charges / `verify.sql` / invariants |
|---|---|---|---|---|---|
| **Redis killed for 31 s** (`redis-down.sh`) | **2,870 × `503 LOCK_UNAVAILABLE`** on new holds (fail closed); 5/5 confirms of holds made before the outage **booked** during it; 0 × 500. Hold p95 for the run 428 ms (the Redis client retries its dial for ~10 s before failing closed) | `RedisLockUnavailable` **fired** | holds work again **1 s** after Redis restarts, by themselves | 1 s | 0 / 0 rows / all 0 |
| **1 of 2 api replicas SIGKILLed** (`api-replica.sh`, `make up-scale`, nginx) | Throughput held: **382 req/s while the replica was dead vs 389 before**. 1 hold got `502` (in flight on the dead replica). 5 confirms in flight were resent once with the **same key** → `409 REQUEST_IN_PROGRESS` (the dead replica's idempotency lease still owned the key, which is the lease doing its job); all 5 were later booked (Tx B / reconciler) | — | replica healthy again 7 s after restart; nginx sends it traffic again (3,449 requests in 30 s) | 1 s | 0 / 0 rows / all 0 |
| **Worker SIGKILLed for 64 s** + 10 gateway timeouts (`worker-kill.sh`) | **10 × `504 PAYMENT_PENDING`** (money taken, answer lost); everything else normal. A backlog of 16 PENDING payments built up with nothing to resolve it | — | after the restart the reconciler resolved every one (all **booked**: 6,139 bookings vs 6,129 k6 saw); every outbox row DONE, every payment final | 0 s after the load (backlog gone 80 s after the restart) | 0 / 0 rows / all 0 |
| **Gateway 3 s per charge for 60 s** (`slow-gateway.sh`) | Confirm p95 **3,066 ms** (k6), all 6,309 confirms 201, 0 × 504 (3 s < `PAYMENT_TIMEOUT` 5 s). **Holds and seat maps unaffected**: p95 hold 24 → 36 ms, seat map 7 → 9 ms (Prometheus, before → during) | — | back to normal as soon as the gateway is | 0 s | 0 / 0 rows / all 0 |
| **Postgres restarted** (`postgres-restart.sh`) | **233 × `503 SERVICE_BUSY`** and 11 × `504 PAYMENT_PENDING` (Tx B lost; resolved later), **0 × 500**. `/readyz` 503 at +1 s, 200 at +3 s | — | holds succeed again **3 s** after the restart began, **without restarting the API** | 10 s | 0 / 0 rows / all 0 |

**A real bug found here:** the first `postgres-restart.sh` run gave **132 × HTTP 500** in 3 seconds. Only timeouts mapped to `503 SERVICE_BUSY`. A refused connection, "the database system is shutting down", SQLSTATE 57P01 or a connection reset fell through to 500. Data was never wrong (0 double charges, `verify.sql` 0 rows); the status code was. Fixed in `8e14723` (`booking.IsDBUnavailable`). Same test after the fix: **0 × 500**, 114 × 503, and recovery 2 s after the restart, with no API restart.

## 6. The bottleneck: measure → change → measure

### Bottleneck 0: the load generator
At ~1,000 RPS, k6 itself used **494% CPU and 1.3 GiB**, more than the API (208%). Two causes:
- every VU received a copy of 40,000 seat ids in its setup data;
- k6 handled every response body, including about 15 KB per seat map that nothing read.

Each show's seat ids are now a `{first, count}` range, checked to be contiguous, and bodies are discarded unless needed. That brought k6 down to **118% CPU and 237 MiB** at the same rate, with 0 dropped iterations (was 412). **Lesson: first check that the tool measures the system and not itself.** Every number in this file was measured after this fix.

### What the profile and the dashboard showed (capacity at 600 actions/s)
- **DB pool saturated:** all 25 connections `acquired` all the time. Acquire wait was 9.3 s per second, so by Little's law about 9 requests were queued for a connection at any moment (`seatlock_db_pool_acquire_wait_seconds_total`).
- **2.45 pool acquires per HTTP request** (`seatlock_db_pool_acquires_total` / requests). A seat map, ~55% of all requests, did **two** round trips: `GetShow` (only to answer 404 for an unknown show) and then the seat query. A hold did `GetShow` + `GetSeat` before its real work.
- **pprof block profile:** apart from waiting on the (deliberately slow) payment gateway, the biggest wait was `pgxpool.(*Pool).Acquire`, **17% of all blocked time**.
- **pprof CPU profile:** the api used only ~1.4 cores, and 38% of that was syscalls (network I/O). It wasn't CPU-bound; it was waiting on Postgres, the busiest process at up to 366% CPU.
- **The seat-map query itself was cheap** (`EXPLAIN ANALYZE`: 0.4 ms, index scans). The cost was the number of round trips, not the SQL.

**Why not just raise `DB_MAX_CONNS`?** Little's law: connections needed = arrival rate × time each one is held. About 1,860 acquires/s × ~13 ms each ≈ 25 connections, exactly the pool size. A bigger pool would let more queries run at once on a Postgres that is already the busiest process on a shared 8-thread machine. That adds contention, not throughput. Using fewer round trips per request lowers the load instead.

### The change (one commit: `9685d95`)
1. **Seat map: one query.** It now starts `FROM shows LEFT JOIN seats ...`: no rows means 404, a show without seats means an empty list.
2. **Hold: one lookup.** `GetSeatInShow` answers show-not-found / seat-not-found / seat-not-in-show in one query, in the same order as before.

The locks, constraints and fail-closed rules didn't change, and every test stayed green (new integration tests cover both queries and their 404s).

### Before / after (same scenario, same machine)
**600 actions/s, below the knee: median of 9 runs each** (a script bug ran 3 × 3 instead of 3; all 9 are equal-condition runs and all passed every correctness check):

| | Before | After | Change |
|---|---|---|---|
| Pool acquires per HTTP request | 2.45 | **1.67** | **−32%** |
| p95 seat map | 20.5 ms | **12.6 ms** | **−39%** |
| p50 seat map | 2.7 ms | 2.0 ms | −26% |
| p95 hold | 53.6 ms | **44.4 ms** | **−17%** |
| p50 hold | 11.3 ms | 9.5 ms | −16% |
| p95 confirm | 216.2 ms | 211.4 ms | ≈ (gateway-bound) |
| RPS delivered | 768.5 | 768.7 | same load |

**800 actions/s, past the knee: interleaved A/B, 3 pairs** (before, after, before, after…, so machine drift hits both sides equally):

| Median (min–max) of 3 | Before | After |
|---|---|---|
| RPS delivered | 914 (839–981) | **964 (922–980)** |
| p95 seat map | 1,293 ms (681–2,258) | **666 ms (557–967)** (−49%) |
| p95 hold | 1,852 ms (1,081–3,438) | **1,344 ms (1,103–1,870)** (−27%) |
| p95 confirm | 2,519 ms (1,497–4,357) | 2,867 ms (2,167–3,874) (+14%, ranges overlap) |
| Pool acquires per request | 2.45 | 1.67 |
| Correctness checks | 3 / 3 PASS | 3 / 3 PASS |

**Past the knee both versions are overloaded** (every request queues), and the run-to-run spread is large, so this table only shows that the change does not make overload worse: seat maps and holds get through faster and slightly more traffic is served; confirms are a little slower because more of the other requests get through and they all queue for the same 25 connections.

**Why interleaved?** A first, non-interleaved run at 800/s (all "before" runs first, then all "after" runs, about an hour later) showed the *after* version **worse** (median p95 hold 1,289 → 3,240 ms, with shedding). Before reverting anything, the plan of the new query was checked (`EXPLAIN ANALYZE`: same indexes) and the comparison was repeated interleaved. With both sides alternating, the difference reversed. The first result was **drift**: the "after" runs came last, after an hour of sustained load on a laptop CPU. That run is kept in `tmp/` and not used. **Lesson: compare A and B under the same conditions, or the comparison measures the machine.**

## Grafana screenshots to take

Dashboard: http://localhost:3001/d/seatlock-reliability?kiosk (1920×1080). Set the time range to the run's window, from the run's `events.txt` / k6 log time, plus 1 minute on each side. Save the files to `docs/images/`.

| File | Scenario | Time window | Panels |
|---|---|---|---|
| `bench-rush.png` | `run.sh rush` | the 10 s run ± 30 s | Correctness row (all 0), RPS by status (the 409 wall), hold/confirm p95 |
| `bench-capacity-knee.png` | `capacity.sh` | the whole ladder (~35 min for 400–800 ×3) | RPS by route, p50/p95/p99 holds climbing step by step, DB pool connections + acquire wait |
| `bench-spike.png` | `run.sh spike` | the 150 s run | RPS (the 10× block), hold p95 up and back down, 5xx = 0 |
| `bench-soak.png` | `run.sh soak` | the 10 min run | Flat lines: DB/Redis pools, outbox backlog, oldest PENDING payment; Correctness row |
| `chaos-redis.png` | `chaos/redis-down.sh` | load start → end | Holds by outcome (`lock_unavailable` spike), Redis lock ops `error`, Alerts row (`RedisLockUnavailable`) |
| `chaos-api-replica.png` | `chaos/api-replica.sh` | load start → end | RPS (flat through the kill), oldest PENDING payment rising and then reconciled |
| `chaos-worker.png` | `chaos/worker-kill.sh` | load start → consistency | Worker: seconds since last success (gap), PENDING payments, reconciler rows by outcome |
| `chaos-slow-gateway.png` | `chaos/slow-gateway.sh` | load start → end | Confirm p95 at ~3 s while hold/seat-map p95 stay flat; gateway latency panel |
| `chaos-postgres.png` | `chaos/postgres-restart.sh` | 30 s before → 60 s after the restart | RPS by status (the 503 blip, no 500), DB pool connections dropping and refilling |
| `bottleneck-before-after.png` | `loadtest/ab.sh` | one before and one after run | DB pool acquire wait and p95 seat map, side by side |

## What these numbers do and don't mean

**They do show:**
- Correctness holds under pressure: 0 double bookings, 0 double charges and `verify.sql` at 0 rows after **every** run, including every chaos run.
- The fail-closed and recovery behaviour works under real concurrency, not only in unit tests.
- Where this design runs out on this hardware, and why (DB round trips and a shared CPU), backed by profiles.

**They don't show:**
- **Production capacity.** One laptop, everything co-located, k6 included. On separate machines (load generator, api, Postgres) the same code would do better. How much better wasn't measured.
- **Scaling.** One api replica, one Postgres. The scale profile (2 replicas) was used for failover, not for throughput.
- **A real network or a real gateway.** Container-to-container latency is microseconds, and the gateway is a mock with a 50–150 ms delay.
- **Long-term stability.** The soak is 10 minutes, not hours.
- **Exact percentiles from Prometheus.** The k6 numbers are exact. Prometheus estimates percentiles from histogram buckets (e.g. a 3 s confirm shows as ~4.9 s, interpolated inside the 2.5–5 s bucket).
- **Repeatability on other machines.** Laptop CPUs throttle; variance between runs is real, so ranges are reported, not only medians.

## Run files

[runs/](runs/) holds the k6 `--summary-export` JSON (and the compact `.result.json`) of every final run, named `<scenario>-<run>.json`.
