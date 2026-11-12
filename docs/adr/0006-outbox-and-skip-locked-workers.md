# ADR-0006: Transactional outbox + `SKIP LOCKED` workers (no leader election)

**Status:** accepted (Phase 3–4)

## Context
Some side effects happen outside Postgres: refunding a charge at the gateway, releasing a Redis lock. If the API does them inline and then crashes, or the call fails, they are lost: money owed is never returned, a lock lingers. Background jobs (reconciling stuck payments, expiring holds) also need to run reliably, and scaling or restarting the worker must never process a row twice.

## Decision
- **Transactional outbox:** a side effect is written as an `outbox` row (`REFUND`, `RELEASE_LOCK`) **in the same transaction** as the decision that causes it. Either both commit or neither does.
- **The worker drains the outbox** and runs the other jobs (reconciler, hold expirer, invariants, key cleanup). Rows are claimed with **`FOR UPDATE SKIP LOCKED` + a lease**: concurrent workers skip rows another worker holds, and a crashed worker's lease simply expires.
- Every side effect is **idempotent** (refund keys, owner-checked lock release), so "at least once" processing has "exactly once" effects.
- Failures retry with backoff `min(2^n, 300) s` + jitter; after 20 attempts a row becomes a `FAILED` **dead letter** for a human (`make outbox-retry ID=…`, alert `OutboxDeadLetters`).

## Consequences
- **No leader election**, no coordinator: run as many workers as you like (`--scale worker=2`). Proven by `TestTwoWorkers_ProcessEveryRowExactlyOnce` and `demo/worker-crash.sh` (SIGKILL mid-backlog → every row done exactly once).
- Chaos-tested: worker killed for 64 s under load with a backlog of 16 PENDING payments → after the restart all were resolved, every outbox row DONE, 0 double charges.
- Side effects are eventually consistent (seconds), which the invariants and alerts watch (`PaymentsStuckPending`, `WorkerJobStalled`).

## Alternatives considered
- **Inline side effects:** lost on crash, no retry.
- **A message broker (Kafka, RabbitMQ):** a second system to keep consistent with Postgres (the dual-write problem the outbox avoids); overkill at this scale.
- **A single worker with leader election:** a single point of failure and extra coordination for no gain over `SKIP LOCKED`.

Details: [phase-4.md](../learning/phase-4.md), [DESIGN.md: worker](../DESIGN.md#the-background-worker).
