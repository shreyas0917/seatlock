# Architecture Decision Records

Short records of the decisions that shape SeatLock: the context, the decision, its consequences and the alternatives that were rejected. They are written from what was actually built and measured (see the [learning notes](../learning/) for the full story).

| ADR | Decision |
|---|---|
| [0001](0001-postgres-is-the-source-of-truth.md) | PostgreSQL is the source of truth; Redis is only a fast lock |
| [0002](0002-fail-closed-when-redis-is-down.md) | Fail closed when Redis is down (Option B: existing holds still confirm) |
| [0003](0003-redis-lock-outlives-db-hold.md) | The Redis lock lives longer than the database hold (310 s vs 300 s) |
| [0004](0004-idempotency-keys-in-postgres.md) | Idempotency keys in PostgreSQL, with leases and lease tokens |
| [0005](0005-no-transaction-across-network-calls.md) | No transaction held across a network call (Tx A → gateway → Tx B) |
| [0006](0006-outbox-and-skip-locked-workers.md) | Transactional outbox + `SKIP LOCKED` workers (no leader election) |
| [0007](0007-void-before-abandoning-a-charge.md) | Void the charge key before declaring a payment abandoned |
| [0008](0008-open-model-load-testing.md) | Open-model load testing, and numbers only with their environment |

Format: **Context → Decision → Consequences → Alternatives considered**.
