# ADR-0004: Idempotency keys in PostgreSQL, with leases and lease tokens

**Status:** accepted (Phase 3)

## Context
"Pay" is the one request users retry the most (double clicks, flaky mobile networks, timeouts) and the one that must never run twice. Each confirm carries an `Idempotency-Key`. The store behind it must be as durable as the payment itself, and must cope with a server that crashes in the middle of a request.

## Decision
- Keys live in the **`idempotency_keys` table** in Postgres, per `(user_id, key)`, with the hash of the request.
- **Claim:** the first request inserts the key `IN_PROGRESS` with a **lease** (`locked_until`, 30 s) and a random **lease token**.
- **Replay:** a finished key returns the **stored response byte for byte** (`Idempotent-Replayed: true`); a key reused with a different body → `422 IDEMPOTENCY_KEY_REUSED`.
- **Concurrent duplicate:** while the lease is live → `409 REQUEST_IN_PROGRESS` ("retry shortly").
- **Crash recovery:** after the lease expires, a retry **takes over** the key with a new token. The original request, if it was only slow, can no longer store its answer: storing requires `lease_token = <its token>`.
- One live payment per hold (partial unique index) and one gateway `charge_key` per payment back this up, so even two different keys can't charge twice.

## Consequences
- No double charge from retries, double clicks or concurrent requests (`TestPay_SameKeyConcurrent_OneChargeOneBooking`, `demo/duplicate-request.sh`).
- A crashed request never blocks its key forever. Seen under chaos: when an API replica was killed mid-confirm, the client's immediate retry got `409 REQUEST_IN_PROGRESS` (the dead replica's lease), and the payment was completed by the reconciler: one charge, one booking.
- The key and the payment are in the same database, so their state can't drift apart.
- Keys are deleted by the worker after 24 h (`IDEMPOTENCY_RETENTION`).

## Alternatives considered
- **Keys in Redis:** fast, but lost on restart (a retry after a Redis restart could charge again), and a second source of truth ([ADR-0001](0001-postgres-is-the-source-of-truth.md)).
- **No lease (lock forever while IN_PROGRESS):** a crash would leave the key stuck.
- **Lease without a token:** a slow original request could overwrite the answer of the retry that took over.

Details: [phase-3.md](../learning/phase-3.md), [internal/idempotency/store.go](../../internal/idempotency/store.go).
