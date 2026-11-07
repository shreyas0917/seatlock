# Phase 2 study notes: seat holds with Redis + PostgreSQL as the authority

Goal of Phase 2: real ticketing is two steps. You **hold** a seat for a few minutes while you pay, then you **confirm**. Phase 2 adds holds, using Redis as a fast lock and PostgreSQL as the final authority, and proves the system stays correct when Redis loses its data or goes down completely.

> **The policy (enforced in code):** SeatLock **fails closed**. Cache unavailability must never result in unauthorized bookings. Existing valid reservations may complete using authoritative database state.

---

## 1. The hold → confirm flow

```
 Client              API (service)              Redis                    PostgreSQL
   │                     │                        │                          │
   │ POST /v1/holds      │                        │                          │
   │ {show 1, seat 7}    │                        │                          │
   │────────────────────>│ validate, show/seat exist? ──────────────────────>│
   │                     │                        │                          │
   │                     │ SET seat:1:7 alice NX EX 310                      │
   │                     │───────────────────────>│                          │
   │                     │<──── OK (or nil = taken, or ERROR = down) ────────│
   │                     │                        │                          │
   │                     │ BEGIN                                             │
   │                     │  SELECT seat FOR UPDATE      (one at a time per seat)
   │                     │  UPDATE stale ACTIVE holds -> EXPIRED  (NOW())    │
   │                     │  seat booked?          -> 409 SEAT_ALREADY_BOOKED │
   │                     │  ACTIVE hold exists?   -> same user: 200 / other: 409
   │                     │  INSERT hold, expires_at = NOW() + 300s           │
   │                     │ COMMIT ──────────────────────────────────────────>│
   │<── 201 {hold_id, expires_at, seconds_left} ─────────────────────────────│
   │                     │                        │                          │
   │ POST /v1/bookings   │                        │                          │
   │ {hold_id}           │       (no Redis call at all)                      │
   │────────────────────>│ BEGIN                                             │
   │                     │  SELECT hold FOR UPDATE                           │
   │                     │  not mine? -> 404 · CONSUMED? -> 409 · expired? -> 410
   │                     │  INSERT booking (UNIQUE show+seat, UNIQUE hold_id)│
   │                     │  UPDATE hold SET status = 'CONSUMED'              │
   │                     │ COMMIT ──────────────────────────────────────────>│
   │                     │ best-effort: release seat:1:7 (if owner) ────────>│
   │<── 201 booking ─────│                        │                          │
```

Hold statuses: `ACTIVE` → `CONSUMED` (confirmed) · `RELEASED` (cancelled) · `EXPIRED` (time ran out).

---

## 2. Why two layers, and why PostgreSQL is the authority

| | Redis | PostgreSQL |
|---|---|---|
| Role | Fast first filter | Source of truth |
| Speed | ~1ms, in memory | slower (disk, transactions) |
| Can lose data? | **Yes**: restart without persistence, `FLUSHALL`, failover to an empty replica | No: committed means durable |
| What it stops | the 49 losers of a 50-user race, **before** they touch the database | every invalid hold and booking, **always** |

**Why have Redis at all?** In a flash sale, thousands of people click the same seat. Without Redis, every click opens a database transaction and queues on the seat's row lock. With Redis, one `SET NX` per click answers "taken" in about 1ms, and only the winner goes on to the database.

**Why not trust Redis?** Because it can forget. If Redis restarts and loses user A's lock, user B's `SET NX` succeeds. If we trusted that, B would get the seat while A still holds it. So the Postgres transaction **re-checks everything** and finds A's ACTIVE hold → B gets 409. This is proven by `TestRedisLostData_PostgresWins` in [service_integration_test.go](../../internal/booking/service_integration_test.go).

**Rule:** Redis can only say *no* early. It can never turn a Postgres *no* into a *yes*.

---

## 3. Fail-closed, explained with a Redis-down timeline

"Fail closed" means: when a safety component cannot be asked, **deny**. (A door with a fail-closed lock stays locked during a power cut.) The opposite, fail open ("Redis is down, so skip the lock"), would hand out holds with no protection at all.

```
time   event                                       result
────   ─────────────────────────────────────────   ─────────────────────────────────────────
t0     Alice holds seat X (Redis up)               201, hold #28 ACTIVE in Postgres
t1     ── Redis goes down ──
t2     Bob tries to hold seat Y                    SET NX -> connection refused / timeout
                                                   -> ErrLockUnavailable -> 503 LOCK_UNAVAILABLE
                                                   -> the repository is NEVER called, no row written
t3     /readyz                                     200 {"postgres":"up","redis":"down"}
                                                   (the API stays in the load balancer)
t4     Alice confirms hold #28                     confirm uses Postgres only -> 201 booking
t5     Carol cancels her hold                      Postgres -> RELEASED -> 200
                                                   (Redis release fails, ignored; lock expires by TTL)
t6     ── Redis comes back ──
t7     Bob holds seat Y again                      201
```

- **New hold (t2): refused.** We can't ask the lock, so we don't know if someone else is racing for that seat. The code has **no flag, fallback or path** that skips the lock. See `HoldSeat` in [service_holds.go](../../internal/booking/service_holds.go) and `Acquire` in [lock/redis.go](../../internal/lock/redis.go).
- **Existing hold (t4): completes.** Alice's hold row in Postgres is the proof of her reservation. Confirming needs nothing from Redis, so it works. That's "Option B": valid reservations may complete using authoritative database state.
- **Why `/readyz` stays 200 (t3):** Redis is shared. If readiness failed whenever Redis was down, the load balancer would remove *every* API instance at the same moment, and confirms, seat maps and lookups would all go down too, although they work fine. Only Postgres decides 503 ([handlers.go](../../internal/httpapi/handlers.go) `readyz`).

Run it for real: `./demo/redis-down.sh`.

---

## 4. Why the Redis TTL is 310s but the DB hold is 300s

The Redis key must **outlive** the database hold, so that whenever Postgres says "held", Redis says so too.

The bug it prevents, if both were exactly 300s:

```
t = 0.000s     Redis: SET seat:1:7 alice EX 300      (lock taken FIRST)
t = 0.040s     Postgres: hold expires_at = NOW()+300  (row written 40ms LATER)
...
t = 300.000s   Redis key expires                      Redis says: seat free
t = 300.010s   Bob: SET NX succeeds                   Bob passes the fast filter
t = 300.012s   Postgres: Alice's hold still ACTIVE    (expires at 300.040s) -> Bob 409
```

Still **safe** (Postgres rejects Bob), but the lock has stopped doing its job, and Bob wasted a database transaction. Clock drift, slow requests and GC pauses make the gap bigger. With `REDIS_LOCK_EXTRA = 10s`, Redis always covers the database hold.

**The price (documented limitation):** for up to 10s after a hold expires, the seat map (from Postgres) shows `AVAILABLE`, but another user's hold attempt can still get `409 SEAT_HELD` from Redis. That errs on the safe side.

**Same-user re-hold refreshes the TTL.** If Alice re-holds a seat she already has (a retry), Postgres may create a fresh 300s hold. So the Lua script `refreshIfOwnerScript` resets the Redis TTL to 310s at the same time. Without it, the old Redis key could die long before the new database hold.

---

## 5. New concepts, and where to find them

### `pgx.Tx` and `defer tx.Rollback(ctx)` · [repository_holds.go](../../internal/booking/repository_holds.go)
A transaction groups statements: all of them happen, or none do. Right after `pool.Begin` we write `defer tx.Rollback(ctx)`. On **any** early return (an error, or a business rule saying no), the deferred Rollback undoes everything. If we reach `tx.Commit`, the deferred Rollback becomes a harmless no-op. So no code path can leave a half-done transaction.

### `SELECT … FOR UPDATE` · [repository_holds.go](../../internal/booking/repository_holds.go)
Reads a row **and locks it** until the transaction ends. Any other transaction that wants to lock the same row waits. Inside `CreateHold` we lock the **seat** row, so all hold attempts for that seat run one at a time. That makes "check, then insert" safe *inside* the transaction (unlike Phase 1's Go-level check-then-insert, which was unsafe). `ConfirmHold` locks the **hold** row, so 10 concurrent confirms run one after another. The first consumes the hold, and the other 9 see `CONSUMED` → 409 (`TestConfirmRace`).

### Partial unique index · [0002_holds.up.sql](../../db/migrations/0002_holds.up.sql)
```sql
CREATE UNIQUE INDEX holds_one_active_per_seat ON holds (show_id, seat_id) WHERE status = 'ACTIVE';
```
A *partial* index only contains rows matching its `WHERE`. So uniqueness applies only among ACTIVE holds: a seat can have any amount of history (CONSUMED / RELEASED / EXPIRED) but **never two ACTIVE holds**. It's a database-level guarantee, even if Go code or Redis is wrong. Because it only holds ACTIVE rows, it stays small.

### `redis.Nil` vs a real error · [lock/redis.go](../../internal/lock/redis.go) `Acquire`
- `redis.Nil`: Redis **answered**, and the answer is "nothing was set" (the key exists, so someone holds the lock). A normal, healthy reply.
- Any other error (connection refused, i/o timeout): we got **no answer**. We know nothing about the seat → `ErrLockUnavailable` → 503.

Mixing these up is exactly how fail-open bugs happen. Treating "no answer" as "nothing there" would mean "Redis down = every seat is free".

### Lua scripts and the owner-checked release · [lock/redis.go](../../internal/lock/redis.go)
Redis runs a Lua script **atomically**: no other command runs between its lines.

```lua
if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end
return 0
```

**Why a plain `DEL` is dangerous:** Alice's lock expires (she was slow), then Bob locks the seat. Now Alice's late `DEL seat:1:7` deletes **Bob's** lock, and Carol can grab the seat while Bob is paying. Compare-then-delete in one atomic script means you can only delete **your own** lock. Doing it as two commands (GET, then DEL) would reopen the gap between them.

### Interfaces for testing · [locker.go](../../internal/booking/locker.go), [service_test.go](../../internal/booking/service_test.go)
`SeatLocker` is declared in the **booking** package (the package that *uses* it), not in `lock`. The service depends on the interface, so unit tests pass a `fakeLocker` that can pretend Redis is down. That's how `TestHoldSeat_LockerError_FailsClosed` proves "the repository is never called" in milliseconds, with no Redis.

### Smaller ones
- **`context.WithoutCancel`** ([service_holds.go](../../internal/booking/service_holds.go) `releaseLock`): keep trying the best-effort release even if the client has already disconnected.
- **Two `%w` in one `fmt.Errorf`** ([repository_postgres.go](../../internal/booking/repository_postgres.go) `dbError`): wraps both `ErrBusy` and the original pgx error, so `errors.Is` finds either.
- **External test package `booking_test`** ([service_integration_test.go](../../internal/booking/service_integration_test.go)): `internal/lock` imports `booking`, so a test *inside* package `booking` can't import `lock` (import cycle). An external test package can.
- **Expand/contract migration**: migration 0002 made `hold_id` required. For a few commits, the old direct-booking code wrote an instant CONSUMED hold, so every commit stayed green until the new endpoints replaced it.

---

## 6. The pool-size fix, and why 503 beats 500

**The problem (found in the Phase 1 review):** pgx's default `MaxConns` is only `max(4, CPUs)`. In a burst, extra requests **queue for a connection**, and that wait counts against their 3s query timeout. Timeouts surfaced as 500 INTERNAL_ERROR.

**The fix:**
1. `DB_MAX_CONNS=25`, `DB_MIN_CONNS=5` ([config.go](../../internal/config/config.go), [pool.go](../../internal/db/pool.go)).
2. Every repository error goes through **one** helper, `dbError`. It turns `context.DeadlineExceeded` (a slow query **or** a pool-acquire timeout) into `ErrBusy` → **503 `SERVICE_BUSY`** with `Retry-After: 1`.
3. `TestHTTP_ParallelHoldBurstHasNo500s`: 50 parallel HTTP requests on the default pool, no warm-up → only 201/409.

**Why 503, not 500?** 500 says "the server is broken": a bug, page someone, don't retry. A timeout under load isn't a bug. The server is healthy but temporarily overloaded, and the same request will likely work in a moment. 503 + `Retry-After` says exactly that: clients back off and retry, and dashboards can tell *overload* apart from *broken code*.

**The same lesson, again, with Redis.** On a cold Redis pool, a 50-request burst opened ~40 new connections at once. On Docker Desktop, their first replies sometimes took over 200ms → `i/o timeout` → `503 LOCK_UNAVAILABLE`. Fail-closed worked correctly, but users got needless 503s. Measured with a probe: **10 warm connections + pool capped at 10 → 0 timeouts in 5 bursts**, versus 25 timeouts on a cold default pool. Hence `REDIS_POOL_SIZE=10`, `REDIS_MIN_IDLE_CONNS=10`, with timeouts still 200ms. A capped pool makes a burst **queue for a warm connection** (Redis answers in ~1ms) instead of dialing dozens of new ones.

**Testing lesson:** integration test packages share one test database. `go test ./...` runs packages **in parallel**, so one package's `TRUNCATE` wiped another's rows mid-test (false `bookings_hold_fk` failures). `make test-integration` uses `-p 1`.

---

## 7. Known limitations

- **Hold hoarding.** A user can re-hold the same seat right after their hold expires, again and again, and so keep a seat indefinitely. Not fixed in Phase 2; a later phase will add a per-user re-hold cooldown or limit.
- **10-second Redis/DB window** (section 4). Up to 10s where the seat map says AVAILABLE but a new hold gets `409 SEAT_HELD`.
- **Lazy expiry.** Expired holds stay `ACTIVE` in the table until the next hold attempt on that seat marks them `EXPIRED`. Every read checks `expires_at > NOW()`, so this is invisible to users. A background sweeper comes with the worker in a later phase.
- **Best-effort Redis release.** If Redis is down during a cancel or confirm, the lock lingers until its TTL. Phase 4's transactional outbox will make the release reliable.

---

## 8. Interview questions (with short answers)

**Q1. What happens if Redis restarts and loses all locks?**
Nothing unsafe. Redis is only a fast filter. A second user may now get the Redis lock, but the hold transaction in Postgres locks the seat row, finds the first user's ACTIVE hold, and returns 409. We even release the second user's Redis lock. Proven by `TestRedisLostData_PostgresWins`.

**Q2. What happens if Redis is completely down?**
New holds fail closed: `SET NX` errors → `ErrLockUnavailable` → 503, and the database is never touched. Existing holds still confirm, because confirm uses only Postgres. `/readyz` stays 200 so instances aren't pulled from the load balancer.

**Q3. Why not just skip Redis when it's down ("fail open")?**
Then the system would hand out holds with no lock in front, exactly when something is already wrong. Postgres would still prevent *double* holds, but fail-open is a policy decision we refuse: an unavailable safety check must mean "no", never "yes".

**Q4. Why is the Redis TTL longer than the database hold?**
So Redis never frees a seat that Postgres still considers held. The lock is taken before the row is written, so with equal TTLs Redis would expire first. The 10s extra covers that gap, plus clock drift and slow requests.

**Q5. Why release the lock with a Lua script instead of `DEL`?**
`DEL` could delete someone else's lock if yours had already expired and they'd taken the seat. The script checks "is the value still me?" and deletes in one atomic step.

**Q6. What does `SELECT … FOR UPDATE` do here?**
It locks the seat row (or hold row) until commit. Concurrent hold attempts for one seat, or confirms for one hold, run one at a time, so the checks that follow inside the transaction can't be raced.

**Q7. What is a partial unique index and why use one?**
A unique index over only the rows matching a `WHERE`. `UNIQUE (show_id, seat_id) WHERE status = 'ACTIVE'` allows unlimited hold history but at most one active hold per seat, guaranteed by the database itself.

**Q8. Ten confirm requests for the same hold arrive at once. What happens?**
`FOR UPDATE` on the hold row serialises them. The first inserts the booking and marks the hold CONSUMED. The other 9 then read CONSUMED → 409 `HOLD_ALREADY_USED`. As a backstop, `UNIQUE (hold_id)` on bookings would also reject a second booking. Proven by `TestConfirmRace` (1 booking, 9 rejections).

**Q9. Why return 404 for someone else's hold, not 403?**
403 confirms the hold exists. An attacker could probe IDs and learn which holds are real. 404 gives the same answer for "doesn't exist" and "not yours".

**Q10. Why are all expiry checks done with `NOW()` in SQL?**
Several API servers may disagree about the time by seconds. They all share one database clock, so `expires_at <= NOW()` gives every server the same answer.

**Bonus. Why does a same-user hold retry return 200 with the same hold?**
Retries after a timeout must be safe. If the first request actually succeeded, the retry returns that hold (200) instead of failing with 409 or creating a duplicate. The Redis side matches: if the lock's owner is the same user, it counts as acquired.

---

## 9. What changes in Phase 3

Phase 3 puts **payment** between hold and confirm. Confirm will call a mock payment service, and every payment request will carry an **idempotency key**, so a retried or duplicated "pay" never charges twice. A **background worker** will appear to handle what requests can't, starting with holds whose payment succeeded too late: if a hold expires after the money was taken, the system must **refund** it instead of booking. The rules from Phase 2 stay: Postgres decides, Redis only filters, and every unsure answer fails closed.
