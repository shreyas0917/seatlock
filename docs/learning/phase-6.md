# Phase 6 study notes: load testing, chaos, and honest numbers

Goal of Phase 6: put **real pressure** on SeatLock and answer two questions with numbers instead of claims:
1. **Does it stay correct?** 0 double bookings and 0 double charges, under a flash sale, a traffic spike, a long run, and five kinds of failure.
2. **How much can it take, and what limits it?** Measured on one laptop, with the environment recorded, and reported honestly.

Results: [docs/benchmarks/RESULTS.md](../benchmarks/RESULTS.md). Code: [loadtest/](../../loadtest), [chaos/](../../chaos).

---

## 1. Load vs stress vs spike vs soak

| Test | Question | SeatLock's version |
|---|---|---|
| **Load** | Does it behave at the traffic we expect? | `rush.js`: a flash sale, 5,000 attempts on 100 seats in 10 s |
| **Stress** (capacity) | Where does it break, and how? | `capacity.js` + `capacity.sh`: step the rate up until latency or errors cross a line (the **knee**) |
| **Spike** | What happens when traffic jumps suddenly, and does it recover? | `spike.js`: 10× for 30 s, then back |
| **Soak** | Does anything leak or pile up over time? | `soak.js`: 10 minutes at half the knee; heap, goroutines, pools and backlog must stay flat |

Chaos tests are different again: **break one dependency while under load** and check the system degrades the way it was designed to, then recovers by itself.

---

## 2. Open vs closed models, and coordinated omission

- **Closed model:** N virtual users in a loop: send, wait for the answer, send again. If the server slows down, each user sends *less often*, so the load **drops exactly when the server struggles**.
- **Open model:** requests **arrive on a schedule** (k6 `constant-arrival-rate`: "start 500 attempts per second"), whether or not earlier ones have finished. Real users behave like this: a thousand people opening the sale page don't wait for each other.

**Coordinated omission** is the measurement bias of the closed model. When the server stalls for 2 s, a closed-model user simply doesn't send the requests it would have sent during those 2 s. Their (terrible) latency is never recorded, and the percentiles look far better than what real users saw. With an open model those requests are still sent and timed.

The catch: an open model needs enough VUs to keep the schedule. If k6 runs out, it **drops** iterations. SeatLock's runs report `dropped_iterations`, and rush/smoke fail on any drop, so "5,000 attempts" really means 5,000.

---

## 3. Why 409 is not an error, and how the thresholds were set

In a flash sale, 4,900 of 5,000 people **must** be told "this seat is taken". `409 SEAT_HELD` and `409 SEAT_ALREADY_BOOKED` are the system **working**. Counting them as errors would give a 98% "error rate" for a perfect run.

So every response gets one of three labels (`loadtest/lib.js`):
- **expected:** 200/201, and 409 *with a seat-conflict code* (a 409 with any other code is unexpected).
- **shed:** `503 SERVICE_BUSY`, the API refusing work it can't finish in time. That's correct *overload* behaviour, so only the capacity test accepts it.
- **unexpected:** everything else. Any **500 is always a bug**, at any load.

Thresholds:
- **Rush, spike, smoke:** `unexpected_status == 0`. Nothing is broken, so any 503 or 500 is a failure.
- **The knee rule:** p95(hold) < 250 ms (the same line as the `SlowHolds` alert from Phase 5) **and** shed + unexpected < 0.1% (also in the spirit of `HighErrorRate`'s 1%, but stricter).
- **Correctness after every run:** exact booking counts, money == bookings + refunds, `verify.sql` 0 rows, invariant gauges 0. A fast run that is wrong is a failed run.

---

## 4. Percentiles, and why p99 matters in a rush

p95 = 95% of requests were faster than this. Averages hide the slow tail (see Phase 5). In a **rush**, the tail matters even more:
- Everyone arrives in the same few seconds, so the slow 1% isn't spread over the day. It's **50 people per second** at 5,000 attempts.
- The slow requests are the *unlucky* ones: they queued behind others for a DB connection. In a sale that's the user staring at a spinner while "their" seat goes to someone else.
- A hold that takes too long can feel like a failure even when it succeeds, so people retry and add load.

That's why the results report p50, p95 **and** p99 per endpoint, plus an end-to-end **hold → booked** time.

---

## 5. Little's law and sizing the DB pool (with our numbers)

**Little's law:** *items in a system = arrival rate × time each spends in it*: L = λ × W.

Applied to the pool at 600 actions/s, **before** the fix:
- λ = **~1,860 pool acquires per second** (from `seatlock_db_pool_acquires_total`)
- W = how long each acquire keeps its connection. With all **25** connections permanently busy: W ≈ 25 / 1,860 ≈ **13 ms**
- The *waiting* line obeys the same law: acquire wait was **9.3 s per second**, so on average **~9 requests were queued** for a connection

What to do with that:
- **Raising the pool** raises L, more queries in Postgres at once. That only helps if Postgres has idle capacity. Here it was the busiest process (up to 366% CPU) on a shared machine, so more connections would mostly mean more contention.
- **Lowering λ**, fewer acquires per request, helps for free: same work, fewer round trips. That's what we did: **2.45 → 1.67 acquires per request (−32%)**.
- **Lowering W** (faster queries) would also help, but `EXPLAIN ANALYZE` showed the queries were already ~0.4 ms. The time was round trips and queuing, not SQL.

A common starting point for pool size is roughly `(CPU cores of the DB × 2) + spindles`: for a 4-core Postgres, that's ~10–25. Measure from there.

---

## 6. How the bottleneck was found (pprof + Grafana)

1. **First, check that the tool measures the system, not itself.** At ~1,000 RPS, k6 used **494% CPU and 1.3 GiB**, more than the API. Every VU got a copy of 40,000 seat ids, and k6 processed 15 KB seat-map bodies it never read. After the fix: 118% CPU and 237 MiB. Every earlier number was thrown away.
2. **The dashboard (Phase 5):** "DB pool connections" showed 25/25 acquired, and "DB acquire wait" was high. Postgres CPU was the highest of any container.
3. **A ratio from Prometheus:** acquires ÷ HTTP requests = **2.45**. Why more than 2 per request, when most requests are simple reads?
4. **pprof CPU profile** (`:9091/debug/pprof/profile?seconds=30`): the API used only ~1.4 cores, 38% of it syscalls. It wasn't CPU-bound; it was waiting.
5. **pprof block profile** (with `PPROF_BLOCK_RATE` on): apart from the gateway wait (deliberate), the biggest wait was `pgxpool.Acquire`, **17% of all blocked time**.
6. **Reading the code on the hot paths:** the seat map did `GetShow` (only to answer 404) + the seat query, and a hold did `GetShow` + `GetSeat` before its real work. Seat maps are ~55% of requests.
7. **The change:** one query each. The 404s and the error order stay identical; new integration tests cover them.
8. **Measure again, same scenario.** 600/s: p95 seat map **−39%**, p95 hold **−17%**, acquires/request −32%. Past the knee (800/s), an **interleaved** A/B (before, after, before, after…) showed seat-map p95 −49% and hold p95 −27%. A first, non-interleaved comparison had shown the opposite. It was drift: the after runs came an hour later on a hot laptop. Measure A and B under the same conditions.

Tools in one line each:
- **CPU profile:** where the CPU time goes.
- **Block profile:** where goroutines *wait* (channels, `select`, locks, pool acquire).
- **Mutex profile:** contended locks.
- **Heap profile:** what holds memory.
- **Goroutine dump:** what everything is doing right now.

---

## 7. What each chaos test proved

| Test | What it proved |
|---|---|
| **Redis killed 30 s** | Fail-closed is real under load: new holds get `503 LOCK_UNAVAILABLE` (2,870 of them in the final run), while **confirms of existing holds still succeed** (5/5). `RedisLockUnavailable` fired, and holds came back by themselves 1 s after Redis did. One surprise: for the first ~10 s, holds got *slow* (p95 ~640 ms) before failing, because the Redis client retries its dial. |
| **API replica killed** | With 2 replicas behind nginx, throughput didn't drop. Requests in flight on the dead replica failed (final run: 1 hold got 502, 5 confirms were resent). The client resent a failed confirm **with the same Idempotency-Key** and got `409 REQUEST_IN_PROGRESS`: the dead replica's idempotency lease still owned the key. That's the lease doing its job, stopping two processes from paying the same booking. The charge had been made, so the **reconciler booked it ~2.5 min later**. 0 double charges. |
| **Worker killed 60 s** | Nothing user-facing broke. A backlog of PENDING payments (from 10 forced gateway timeouts) waited, and after the restart it **drained exactly once**: every payment booked or refunded, every outbox row DONE, 0 double charges. |
| **Slow gateway (3 s)** | Confirms took ~3 s, but **holds and seat maps didn't slow down at all**, because a confirm holds no DB connection while it waits for the gateway (Tx A → gateway → Tx B, Phase 3). |
| **Postgres restarted** | Found a **real bug**: 132 × 500 in 3 seconds. Fixed so it answers 503 (`IsDBUnavailable`). After the fix: 0 × 500, `/readyz` went 503 → 200, and holds worked again 2 s after the restart, **without restarting the API**. |

The common thread: **every chaos run ended with `verify.sql` at 0 rows and money == bookings + refunds.** Failures cost users some 503s and slow answers, never wrong data.

---

## 8. How to talk about these numbers honestly

- **Say what was measured, in its own words.** "5,000 booking attempts arriving in a 10-second burst", not "5,000 concurrent users". "≈ 514 requests per second on a 4-core laptop with everything on it, including the load generator", not "handles 500+ RPS".
- **Always give the machine.** A number without its environment means nothing.
- **Lead with correctness, then speed.** For a booking system, "0 double bookings under 5 failure scenarios" is the headline. Throughput comes second.
- **Medians and ranges, not best runs.** If someone asks for the best run, say it's the best run.
- **Know your limits, and say them before you're asked.** "Single machine, single replica, mock gateway, 10-minute soak. Here's what I'd do next."
- **Have the story ready:** measured → found → changed → measured again, with the profile that justified the change.

---

## 9. Interview questions (with short answers)

**Q1. How did you load test it?**
With k6, in Docker inside the Compose network, using open-model arrival rates so a slow server can't hide behind fewer requests. Four tests: a flash-sale rush (5,000 attempts on 100 seats in 10 s), a capacity ladder to find the knee, a 10× spike, and a 10-minute soak. After every run a script checks correctness: exact booking counts, money == bookings + refunds, `verify.sql` 0 rows, and invariant gauges 0.

**Q2. How many requests per second can it handle, and on what machine?**
About **514 HTTP requests per second** (400 user actions/s) with p95 hold 86 ms and 0 errors: the conservative knee. The ladder also passed at 600 actions/s (768 RPS), but 500 failed in all six of its runs on a throttling laptop CPU, so I quote 400 and explain why. That's on a 4-core/8-thread laptop i5 with 8 CPUs given to Docker, where Postgres, Redis, the API, Prometheus, Grafana *and k6 itself* all shared those CPUs. It's a lower bound on that hardware, not production capacity.

**Q3. What was the bottleneck?**
First the load generator itself, at more than twice the API's CPU (494% vs 208%), which I fixed before measuring anything. Then DB pool round trips: all 25 connections busy, about 9 requests queued for one, 2.45 acquires per request. Seat maps (~55% of requests) and holds each made one avoidable extra query. Folding them into single queries cut acquires per request by 32%; p95 seat map fell 39% and p95 hold 17% at 600 actions/s.

**Q4. Why didn't you just increase the connection pool?**
Little's law: 1,860 acquires/s × 13 ms ≈ 25 connections, the pool was exactly full. But Postgres was the busiest process on a shared machine, so more connections would mean more concurrent queries competing for the same CPU. Cutting round trips reduces the work instead of queuing more of it.

**Q5. What's coordinated omission?**
In a closed-loop test, a stalled server makes the virtual users send fewer requests, so the slowest moments are under-recorded and the percentiles look better than reality. Arrival-rate (open) models avoid it: requests arrive on schedule regardless.

**Q6. Why is a 409 not an error in your load test?**
Because in a sale almost everyone *must* be told "seat taken". It's the correct answer. I classify responses as expected / shed / unexpected, and only unexpected ones (any 500, a 503 where there shouldn't be one) fail a run.

**Q7. What happens to bookings when Redis dies under load?**
New holds fail closed with 503 LOCK_UNAVAILABLE, so nobody can grab a seat without the lock. Confirms of holds made before the outage still succeed, because they don't need Redis. The alert fires, and holds recover by themselves about 2–3 s after Redis is back. 0 invariant violations.

**Q8. What did chaos testing find that unit tests didn't?**
A Postgres restart under load gave 132 × 500 in 3 seconds: only timeouts were mapped to 503, not refused connections or "database is shutting down". The data was never wrong, but clients were told "internal error" instead of "retry". It was a one-function fix, with a test per error kind; afterwards, 0 × 500.

**Q9. A server dies in the middle of a payment. What happens?**
Tested: an API replica was SIGKILLed after charging the card but before recording the booking. The client's retry with the same Idempotency-Key got 409 REQUEST_IN_PROGRESS, because the dead replica's lease still held the key, so no second charge was possible. The payment stayed PENDING, and the reconciler asked the gateway and booked the seat ~2.5 minutes later. One charge, one booking.

**Q10. How does CI protect against a double booking?**
A "Load smoke" workflow on every push and PR starts the real Compose stack and sends 200 booking attempts at 20 seats. The build fails unless there are exactly 20 bookings, 0 unexpected statuses, gateway charges == bookings + refunds, and `verify.sql` returns 0 rows. It takes about 1m45s.

**Bonus: what would you do next for real capacity numbers?**
Run k6 on its own machine, Postgres on its own, two or more API replicas, and a multi-hour soak. Add a read replica or a short cache for seat maps (~55% of requests) if they stay the hottest path.

---

## Resume bullets (real medians, exact wording)

XYZ format ("accomplished X, as measured by Y, by doing Z"). Every number is a median from [RESULTS.md](../benchmarks/RESULTS.md), and the wording matches what was measured:

- **Built a ticket-booking backend in Go with zero double bookings under a flash-sale load test:** 5,000 booking attempts in a 10-second burst on 100 seats produced exactly 100 bookings and 100 charges in 3 of 3 runs (p95 hold 15 ms). It uses fail-closed Redis seat locks, PostgreSQL constraints as the authority, and idempotent payments, and was measured with k6 on a single 4-core laptop.
- **Cut database round trips per request by 32% (2.45 → 1.67)**, lowering p95 seat-map latency by 39% and p95 hold latency by 17% at 600 actions/s (median of 9 runs). The bottleneck was found with pprof block profiles (17% of blocked time in connection-pool acquire) and Prometheus pool metrics, then verified with an interleaved before/after A/B.
- **Verified recovery from 5 failure modes under load** (Redis killed, an API replica killed, the worker killed, a 3-second payment gateway, a Postgres restart) with 0 double charges and 0 invariant violations, using chaos scripts that check consistency after every fault. One test exposed 132 HTTP 500s during a Postgres restart; the fix returns 503 and recovers by itself in 3 s.
- **Added a CI load gate** that fails the build on any double booking: a k6 smoke test (200 booking attempts on 20 seats against the full Docker Compose stack) on every push and pull request, finishing in under 2 minutes.

If asked "how many requests per second?": "about 514 requests/s at p95 86 ms for the realistic mix, on one 4-core laptop that also ran the database, Redis, monitoring and the load generator, so a lower bound, not production capacity."
