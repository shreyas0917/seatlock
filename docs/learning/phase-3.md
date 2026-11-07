# Phase 3 study notes: payments, idempotency, refunds

Goal of Phase 3: confirming a hold now **takes money**. That brings three new dangers:
1. **Charging twice**: the user double-clicks "Pay", or the phone retries after a timeout.
2. **Charging without a seat**: the money is taken, but the booking is never saved.
3. **Selling a seat that expired**: the hold runs out while the payment is still in flight.

Phase 3 handles all three while keeping the Phase 2 rule: **fail closed, PostgreSQL is the authority, and confirm works without Redis.**

---

## 1. The confirm sequence: Tx A → gateway → Tx B

```
 Client                 API                        PostgreSQL                 Payment gateway
   │ POST /v1/bookings   │                              │                            │
   │ Idempotency-Key: k1 │                              │                            │
   │ {hold_id: 12}       │                              │                            │
   │────────────────────>│ claim key k1 (IN_PROGRESS, lease 30s) ──────────>│        │
   │                     │                              │                            │
   │                     │ Tx A: BEGIN                  │                            │
   │                     │   SELECT hold FOR UPDATE     │                            │
   │                     │   owner? active? not expired?│                            │
   │                     │   reuse PENDING payment, or INSERT one (charge_key pay-5) │
   │                     │ COMMIT  ───────────────────> │   (locks released here)    │
   │                     │                              │                            │
   │                     │ POST /charges {idempotency_key: pay-5}  ─────────────────>│
   │                     │        ...  NO transaction open, NO row locked  ...       │
   │                     │ <──────────────── succeeded / declined / (no answer) ─────│
   │                     │                              │                            │
   │                     │ Tx B: BEGIN                  │                            │
   │                     │   lock hold, then payment    │                            │
   │                     │   SUCCEEDED already? -> same booking                      │
   │                     │   hold valid?  -> INSERT booking, hold CONSUMED, payment SUCCEEDED
   │                     │   hold expired -> payment REFUND_PENDING + outbox REFUND row
   │                     │ COMMIT  ───────────────────> │                            │
   │                     │ (refund inline if owed) ─────────────────────────────────>│
   │                     │ store final response for k1 (COMPLETED) ────>│            │
   │<── 201 / 402 / 410 / 504 ───────────────────────────│              │            │
```

Files: [handlers_confirm.go](../../internal/httpapi/handlers_confirm.go) → [service_payments.go](../../internal/booking/service_payments.go) → [repository_payments.go](../../internal/booking/repository_payments.go), [idempotency/store.go](../../internal/idempotency/store.go), [payment/client.go](../../internal/payment/client.go).

---

## 2. What idempotency means: the double-click

**Idempotent** = doing it twice has the same effect as doing it once.

```
time  browser                                   server
t0    user clicks "Pay"   -> request #1 (key k1) starts charging...
t1    user clicks again   -> request #2 (key k1) -> 409 REQUEST_IN_PROGRESS (k1 is leased)
t2                                               #1 finishes: 201, response stored for k1
t3    network blip, app retries -> request #3 (key k1) -> stored 201 replayed,
                                                    header Idempotent-Replayed: true
```

One booking, one charge. The client generates the key once per **logical** action ("pay for hold 12") and reuses it on every retry of that action.

Rules (in [idempotency/store.go](../../internal/idempotency/store.go)):

| Situation | Answer |
|---|---|
| new key | claim it (`IN_PROGRESS`, lease 30s) and run |
| same key, **different request** (other `hold_id`; checked with a SHA-256 *request hash*) | **422** `IDEMPOTENCY_KEY_REUSED` |
| same key, still running (lease valid) | **409** `REQUEST_IN_PROGRESS` |
| same key, lease expired (the server crashed mid-request) | take it over and run again |
| same key, `COMPLETED` | replay the stored status + body **byte for byte** |

Only **final** answers are stored: 201, 402, 404, 409, 410. Unknown outcomes (504, 503, 500) **give the key back**, so a retry runs again and can reach a final answer.

**Leases and lease tokens.** A crashed request never calls "complete", so a key would stay `IN_PROGRESS` forever. The **lease** (`locked_until`) fixes that: after 30s, a retry may take the key over. But then **two requests can run for one key**: the slow original and the retry. So every claim gets a random **lease token**, and only `UPDATE … WHERE lease_token = $mine` may store the answer. The slow original finds 0 rows updated and does not overwrite anything. Running twice is still safe because of the layers below: one live payment per hold, one charge key per payment, and Tx B never refunding a booked payment.

**JSON vs JSONB (a bug found by the smoke test).** The stored body was first a `JSONB` column. JSONB stores a *parsed* form: it **reorders keys and changes spacing**. Replays were equal in meaning but not byte-identical. Migration `0004` switched it to `JSON`, which stores the text verbatim. The unit tests had used an in-memory fake and could not catch this; only the real database did.

---

## 3. Why the keys live in PostgreSQL, not Redis

Phase 2's "Option B": **confirm must work while Redis is down.** If idempotency keys lived in Redis, a Redis outage would leave two bad choices:
- refuse every confirm (breaks Option B), or
- confirm **without** duplicate protection (risks charging twice).

In PostgreSQL, the key lives and dies with the payment and booking it protects. Proven by `TestPay_RedisDown_ConfirmStillWorks`: new holds get 503, but confirm-with-payment gets 201.

---

## 4. The four payment outcomes, and why "timeout ≠ failed"

| Gateway says | What we do | HTTP |
|---|---|---|
| **succeeded**, hold still valid | booking + hold `CONSUMED` + payment `SUCCEEDED` (Tx B) | **201** |
| **succeeded**, hold expired during payment | payment `REFUND_PENDING` + outbox `REFUND` (Tx B), refund inline | **410** + `"payment":"REFUNDED"` |
| **declined** | payment `DECLINED`, hold stays `ACTIVE` | **402** (retry with a new key) |
| **no answer** (timeout, connection error, 5xx) | payment stays **`PENDING`** | **504** (retry with the same key) |

**Why a timeout must NOT be treated as "failed":**

```
t0  we send "charge pay-5"
t1  the gateway takes the money
t2  the network drops the reply          <- we time out here
```

If we marked the payment `FAILED` and released the hold, the user would be charged with no seat and no refund. We **don't know**, so we say so (504), keep it `PENDING`, and ask again **with the same `charge_key`**. The gateway is idempotent too: it returns the stored "succeeded" instead of charging again. Demo: `./demo/payment-timeout.sh` (504 → retry → 201, gateway shows 1 charge, 2 requests).

**Decline vs no answer.** A decline is a *definite* answer: no money moved. That's why the hold stays and a **new** payment (new `charge_key`) is allowed. The partial unique index `payments_one_live_per_hold` only covers `PENDING`/`SUCCEEDED`.

**Two confirms, two different keys, one hold** (two browser tabs). Tx A finds the hold's existing `PENDING` payment and **reuses** it, `charge_key` included, so the gateway sees one charge. In Tx B, whoever comes second sees the payment already `SUCCEEDED` and gets **the same booking back**. **A payment that backs a booking is never refunded** (`TestPay_TwoKeysSameHoldConcurrent_OneBookingNoRefund`).

---

## 5. The outbox pattern, with a crash timeline

Some side effects live in **other systems**: refund money at the gateway, delete a Redis lock. They can't be in our database transaction.

**Without an outbox:**
```
1. COMMIT: payment -> REFUND_PENDING
2. --- server crashes ---
3. POST /refunds                          <- never happens
```
The user paid, has no seat, and **nothing remembers** that a refund is owed.

**With an outbox:**
```
1. BEGIN
     UPDATE payments SET status = 'REFUND_PENDING'
     INSERT INTO outbox (kind, payload) VALUES ('REFUND', {payment_id, charge_id, refund_key})
   COMMIT                                 <- both or neither
2. --- server crashes ---
3. the Phase 4 worker finds the PENDING outbox row and does the refund (same refund_key -> paid once)
```
The debt is **written down atomically with the decision**. Inline we still try right away (the user gets their money back now); the outbox is the guarantee. The same goes for cancelling a hold: the `RELEASED` status and a `RELEASE_LOCK` outbox row are committed together.

---

## 6. Never hold a transaction across a network call

If Tx A stayed open during the gateway call (up to 5s), the hold row would stay **locked**. Every retry, cancel or hold attempt for that seat would queue behind it, holding a database connection each. A slow gateway would become a database pile-up.

So: **Tx A commits before the call, Tx B starts after it**. In between, safety comes from **state stored in rows** (payment `PENDING`), not from locks. Two more details:
- **Lock order:** every transaction locks the hold first and the payment second, so they can never deadlock.
- **Proof:** `TestPay_NoTransactionOpenDuringGatewayCall` makes the gateway slow, and *during* the call checks:
  - `SELECT … FOR UPDATE NOWAIT` on the hold and seat **succeeds** (it would fail instantly if they were locked);
  - `pg_stat_activity` shows no `idle in transaction` session (filtered to the test database, excluding the test's own connection);
  - a cancel gets `409 PAYMENT_IN_PROGRESS` (we don't cancel mid-payment).

(Why not just "a seat-map read stays fast"? In Postgres, plain `SELECT`s are **never** blocked by row locks (MVCC), so that test would pass even with the bug.)

---

## 7. Lessons from step 0 and from debugging this phase

- **`set -o pipefail`.** `make test | grep ok && git push` once pushed failing code: a pipeline's exit status is the **last** command's (`grep`). `scripts/verify.sh` uses `set -euo pipefail`, saves the **unfiltered** log to `tmp/`, and is the only push gate.
- **CI runs the same script**, with `-race` on Linux (the Windows machine has no cgo). Local checks and CI cannot drift apart.
- **Correctness tests are not timing tests.** "Exactly one booking" is a *correctness* property and must not fail because a laptop is slow. Integration tests use generous timeouts (DB 15s, Redis 2s). *Latency* (p95 under load) is measured separately by Phase 6's load tests.
- **Measure before you fix.** Intermittent 15s stalls looked like network problems, but probes showed TCP connects were fast. The real cause was **memory pressure** in the shared Docker VM (Linux PSI: all processes stalled ~30% of the time), caused by another project's Kubernetes cluster. A "fix" (connect timeout) made it worse and was reverted.
- **Test the real thing.** Two bugs here were invisible to unit tests with fakes and only showed up against the running stack: the JSONB replay difference, and a dead hold's Redis lock not being released after a refund.

---

## 8. Interview questions (with short answers)

**Q1. How do you avoid charging a customer twice?**
Three layers. (1) The client sends an `Idempotency-Key`; duplicates get the stored response. (2) A hold has at most one live payment (partial unique index), so concurrent confirms share it. (3) Each payment has one `charge_key`, and the gateway charges a key once. On top, Tx B never refunds a payment that backs a booking.

**Q2. What if your server crashes after charging but before saving the booking?**
The payment row is still `PENDING` (committed in Tx A, before the charge). The client's retry with the same key (after the lease expires) runs Tx A again, which reuses that payment and its `charge_key`. The gateway replays "succeeded", and Tx B books. If nobody retries, Phase 4's worker reconciles `PENDING` payments by asking the gateway (`GET /charges/{key}`).

**Q3. Why is a timeout not a failure?**
The gateway may have taken the money and only the reply was lost. Marking it failed could leave the user charged with no seat. We keep it `PENDING`, answer 504, and resolve it by asking again with the same key.

**Q4. Why store idempotency keys in Postgres instead of Redis?**
Confirm must work when Redis is down. In Redis, an outage would force a choice between refusing confirms and confirming without duplicate protection. In Postgres, the keys share the fate of the data they protect.

**Q5. What is a lease, and why do you need a lease token too?**
The lease (`locked_until`) lets a retry take over a key whose request crashed. After a takeover, two requests can run for one key, so the token makes sure only the current owner can store the answer (`UPDATE … WHERE lease_token = $mine`).

**Q6. What happens if the same key is sent with a different body?**
422 `IDEMPOTENCY_KEY_REUSED`. We compare a SHA-256 hash of method, path and the decoded body. Replaying the old answer for a different request would be wrong.

**Q7. Why must you not hold a database transaction during the payment call?**
The rows stay locked for the whole network call. Retries, cancels and holds on that seat queue up, each holding a connection, so a slow gateway exhausts the pool. Commit before the call; protect the gap with row state (`PENDING`).

**Q8. Explain the transactional outbox.**
Write the side effect you owe (refund, lock release) as a row **in the same transaction** as the state change. A crash can't separate "we decided to refund" from "we remember to refund". A worker processes the rows until they are done. The side effects themselves must be idempotent (refund key `refund-<payment_id>`), because they may run more than once.

**Q9. The hold expires while the gateway is charging. What happens?**
Tx B sees the expired hold: no booking, payment `REFUND_PENDING`, outbox `REFUND`, hold `EXPIRED`, all in one commit. Then an inline refund, then 410 with `"payment":"REFUNDED"` (or `REFUND_PENDING` if the refund call failed and the worker must finish it).

**Q10. Two tabs confirm the same hold with different keys at the same moment. What prevents a double charge or a refund?**
Both Tx A calls lock the hold row in turn; the first creates the `PENDING` payment, the second reuses it, `charge_key` included. The gateway charges once. Both Tx B calls lock hold then payment; the first books, the second sees `SUCCEEDED` and returns the same booking. A booked payment is never refunded.

**Bonus. Why 402 for a decline but 504 for a timeout?**
402 is a final answer (no money taken, hold kept, retry with a new key) and is stored for replay. 504 means "unknown", is not stored, and the client should retry with the **same** key.

---

## 9. What changes in Phase 4

Phase 4 adds the **background worker** that finishes what requests leave behind. It marks expired holds `EXPIRED` (instead of only lazily) and **reconciles abandoned `PENDING` payments**: payments whose client never retried after a 504. It asks the gateway (`GET /charges/{charge_key}`) and either books or refunds. It **drains the outbox**: retrying `REFUND` and `RELEASE_LOCK` rows with backoff until they are `DONE`. It also prunes old idempotency keys. Every one of these steps must be idempotent, because the worker can crash and repeat work, and several workers may run at once (`SELECT … FOR UPDATE SKIP LOCKED`).
