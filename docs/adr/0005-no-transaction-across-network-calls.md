# ADR-0005: No database transaction is held across a network call (Tx A → gateway → Tx B)

**Status:** accepted (Phase 3)

## Context
Confirming a booking means charging a card at an external gateway. The simple version (open a transaction, lock the hold, call the gateway, insert the booking, commit) holds a row lock **and** a pooled connection for the whole gateway call. With a 5 s gateway timeout and a 25-connection pool, 25 slow payments would freeze every request in the system.

## Decision
Split the confirm into two short transactions around the call:
1. **Tx A** (`BeginPayment`): check the hold, create or reuse **one `PENDING` payment** for it, commit.
2. **Gateway call, outside any transaction**, with the payment's own idempotent `charge_key` and a 5 s timeout.
3. **Tx B** (`CompletePayment`): record the outcome. On success create the booking; if the hold died meanwhile, mark the payment `REFUND_PENDING` and write a `REFUND` outbox row in the same transaction. Tx B never refunds a payment that already backs a booking.

A timeout is **not** a failure: the payment stays `PENDING` and the client gets `504 PAYMENT_PENDING` (retry with the same key). A retry, or else the worker's reconciler, asks the gateway what really happened.

## Consequences
- Measured (`chaos/slow-gateway.sh`): with a 3 s gateway, confirm p95 rose to ~3 s, while **hold and seat-map latency didn't move** (p95 hold 24 → 36 ms, seat map 7 → 9 ms). No connection is held while waiting.
- More states to handle (`PENDING`, `REFUND_PENDING`, lost answers), all covered by the reconciler, the outbox and the invariants (e.g. "no `PENDING` payment older than 15 min").
- A crash between the gateway and Tx B leaves a `PENDING` payment; the reconciler finishes it (seen in the API-replica chaos test).

## Alternatives considered
- **One transaction around the call:** simplest code, but couples database capacity to the gateway's latency.
- **Two-phase commit / XA with the gateway:** payment gateways don't take part in it.
- **Charge first, then write everything:** a crash after the charge would leave money taken with no record of it.

Details: [phase-3.md](../learning/phase-3.md), [RESULTS.md §5](../benchmarks/RESULTS.md#5-chaos-under-load).
