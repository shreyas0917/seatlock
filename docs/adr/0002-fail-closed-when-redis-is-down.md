# ADR-0002: Fail closed when Redis is down (Option B: existing holds still confirm)

**Status:** accepted (Phase 2)

## Context
When the API can't reach Redis it can't take the seat lock. Three options:
- **A. Fail open:** skip the lock and go straight to Postgres. Correct (Postgres is the authority, [ADR-0001](0001-postgres-is-the-source-of-truth.md)), but the moment Redis dies in a rush, the full stampede lands on Postgres.
- **B. Fail closed for new holds only:** refuse new holds, but let users who **already hold** a seat pay and book.
- **C. Fail closed for everything:** refuse holds and confirms.

## Decision
**Option B**, as a written policy: *"Cache unavailability must never result in unauthorized bookings. Existing valid reservations may complete using authoritative database state."*
- `POST /v1/holds` with Redis unreachable → **`503 LOCK_UNAVAILABLE`** (temporary; clients may retry).
- `POST /v1/bookings` never touches Redis: the confirm/payment path uses Postgres only, so it keeps working.
- Redis timeouts are short (200 ms dial/read/write) so a dead Redis fails fast instead of piling requests up.
- `/readyz` stays 200 when only Redis is down (the API is still useful), 503 only when Postgres is down.

## Consequences
- During a Redis outage nobody can start a new booking, but nobody who already holds a seat loses it. Measured under load (`chaos/redis-down.sh`): 2,870 × 503 on new holds, 5/5 confirms of existing holds booked during the outage, holds back 1 s after Redis returned, 0 invariant violations.
- The `RedisLockUnavailable` alert turns "correctly refusing" into "page a human to bring Redis back".
- Observed cost: for the first ~10 s of an outage, holds get slower (the Redis client retries its dial) before they fail closed.

## Alternatives considered
- **A (fail open)** protects availability but turns a cache outage into a database overload in the exact moment of peak load.
- **C (refuse confirms too)** would cancel sales that are already safe in Postgres, for no correctness gain.

Details: [phase-2.md §3](../learning/phase-2.md), [RUNBOOK: RedisLockUnavailable](../RUNBOOK.md#redislockunavailable).
