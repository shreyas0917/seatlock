# ADR-0007: Void the charge key before declaring a payment abandoned

**Status:** accepted (Phase 4)

## Context
The reconciler finishes `PENDING` payments nobody came back for (e.g. a 504 the user never retried). It asks the gateway "what happened to `pay-<id>`?". If the gateway says **"no charge"**, the tempting move is to mark the payment `ABANDONED` and free the seat.

But "no charge **yet**" isn't "no charge **ever**". A slow request, or a client retry with the old `Idempotency-Key`, may still be on its way to the gateway with that very key. If it lands after we gave up, money is taken and nobody will ever book or refund it.

## Decision
Before abandoning, **close the key at the gateway** (`POST /charges/{key}/void`). The gateway serialises void and charge for one key, so exactly one of two things happens:
- **The void wins:** the key is closed, any later charge with it is refused (no money taken), and `ABANDONED` is safe.
- **The late charge won** (it arrived just before the void): the void answers "succeeded", and the reconciler takes the normal succeeded path (book the seat, or refund via the outbox if the hold is gone).

A new payment for the same hold uses a new `charge_key`, so the user can simply try again (`504 PAYMENT_NOT_COMPLETED`: "no money was taken, please retry").

## Consequences
- Closes the "late charge after abandon" hole: `TestReconciler_NotFound_VoidsAbandons_ThenNewPaymentWorks` (late charge refused, new payment → one successful charge in total) and `TestReconciler_VoidRace_LateChargeWins_Books` (the late charge beats the void → booked, nothing lost).
- Requires a gateway that supports closing a key; the mock implements it. With a real provider this maps to cancelling or expiring the payment intent.
- One extra gateway call, only on the rare abandon path.

## Alternatives considered
- **Abandon on "not found":** loses money in the race described above.
- **Never abandon (keep waiting):** the seat would stay blocked forever after a lost request.
- **Wait longer before abandoning:** narrows the race but never closes it.

Details: [phase-4.md](../learning/phase-4.md), `ReconcilePayment` in [internal/booking/service_worker.go](../../internal/booking/service_worker.go).
