# ADR-0001: PostgreSQL is the source of truth; Redis is only a fast lock

**Status:** accepted (Phase 1–2)

## Context
In a flash sale, hundreds of users try to hold the same few seats in the same second. We need the race losers turned away quickly, but above all we need **one seat → at most one booking**, even when parts of the system misbehave. Redis is fast but can lose keys (eviction, restart, failover, `FLUSHDB`). A booking system cannot lose a sale to that, or sell a seat twice.

## Decision
- **PostgreSQL decides everything** that matters: holds, hold expiry (always the database clock, `NOW()`), payments, idempotency keys, the outbox and bookings.
- **Redis holds one thing only**: a short-lived seat lock (`SET seat:{show}:{seat} <user> NX EX 310`), checked *before* the Postgres hold transaction, to turn away most race losers in ~1 ms without touching the database.
- A Redis "yes" is never enough on its own: the hold transaction re-checks in Postgres (`SELECT … FOR UPDATE` on the seat + the partial unique index `holds_one_active_per_seat`). A Redis "no" is trusted only because it is the cheaper, safe answer.
- Three layers, of which **only the last is the guarantee**: Redis lock → one ACTIVE hold per seat → `UNIQUE(show_id, seat_id)` on `bookings`.

## Consequences
- A double booking is impossible even if Redis lies: proven by `TestRedisLostData_PostgresWins` (Redis flushed, user B gets the Redis lock, Postgres rejects B).
- Every hold still costs one Postgres transaction for the winner; Redis only saves the losers' transactions.
- The seat map is read from Postgres only (Redis may be stale), so it is always truthful.
- Redis and Postgres can briefly disagree (see [ADR-0003](0003-redis-lock-outlives-db-hold.md)); the disagreement is always on the safe side.

## Alternatives considered
- **Redis as the source of truth for holds** (store the hold only in Redis, write Postgres at booking time): fastest, but a Redis restart would forget who holds what, and a seat could be sold to two people.
- **Postgres only, no Redis**: correct and simpler, but every race loser would cost a row lock and a transaction; under a rush that load lands on the one component that must not fall over.
- **Distributed lock (Redlock) as the authority**: more moving parts, and a lock service still can't replace a database constraint as the final word.

Details: [phase-2.md §2](../learning/phase-2.md), [DESIGN.md](../DESIGN.md#how-the-guarantees-are-enforced).
