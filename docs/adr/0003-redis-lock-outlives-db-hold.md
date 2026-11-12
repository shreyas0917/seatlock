# ADR-0003: The Redis lock lives longer than the database hold (310 s vs 300 s)

**Status:** accepted (Phase 2)

## Context
A hold lasts 300 s, decided by Postgres (`expires_at = NOW() + 300s`). The Redis lock is taken **first**, a few milliseconds before the Postgres row is written. If both lived exactly 300 s, Redis would free the seat slightly **before** Postgres does:

```
t = 0.000s     Redis: SET seat:1:7 alice EX 300      (lock taken first)
t = 0.040s     Postgres: hold expires_at = NOW()+300  (row written 40 ms later)
t = 300.000s   Redis key expires                      -> Bob's SET NX succeeds
t = 300.012s   Postgres: Alice's hold still ACTIVE    -> Bob 409
```

Still safe (Postgres rejects Bob), but the lock has stopped doing its job, and clock drift, slow requests and GC pauses widen the gap.

## Decision
- Redis lock TTL = `HOLD_TTL + REDIS_LOCK_EXTRA` = **300 s + 10 s**: whenever Postgres says "held", Redis says so too.
- A same-user re-hold refreshes the Redis TTL (owner-checked Lua script), so a fresh 300 s database hold never outlives its lock.
- After a hold expires or is cancelled, the worker (or the API) releases the lock early through the outbox, with an owner check, so the extra 10 s is usually not felt.

## Consequences
- The fast filter keeps working at the edges of a hold's life.
- **Known limitation:** for up to 10 s after a hold expires, the seat map (Postgres) shows `AVAILABLE` while another user's hold can still get `409 SEAT_HELD` from Redis. It errs on the safe side.
- The lock's value is the **user**, not the hold: a stale release could, in a tiny window, delete the same user's newer lock (Phase 5 narrowed it with a Postgres check before release). A hold-scoped token would close it fully.

## Alternatives considered
- **Equal TTLs:** simpler, but makes the lock leaky exactly when seats turn over.
- **No TTL, explicit release only:** a crashed API would leave seats locked forever.
- **Shorter Redis TTL:** strictly worse; Redis would free seats Postgres still holds.

Details: [phase-2.md §4](../learning/phase-2.md), [internal/lock/redis.go](../../internal/lock/redis.go).
