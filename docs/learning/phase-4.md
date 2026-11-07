# Phase 4 study notes: the background worker

Goal of Phase 4: the request path is fast and safe, but it **leaves things behind**: payments whose answer was lost, refunds that failed, holds nobody cleaned up, keys that pile up. Phase 4 adds `cmd/worker`, a separate process that **repairs all of it**. It is safe to run many copies at once, and safe to kill at any moment.

> Same rules as before: **fail closed, PostgreSQL is the authority, Redis never overrides it.** The worker reuses the API's code paths; there is **no second copy of the booking/refund rules**.

---

## 1. The four jobs, and what goes wrong without each

### Job 1: Payment reconciler (every 30s)
**Story without it:** Alice clicks Pay. The gateway takes ₹250, but the reply is lost; Alice sees 504 and closes the app. Her payment row stays `PENDING` **forever**: she paid, has no seat, gets no refund, and the seat is blocked.

**What it does:** for each `PENDING` payment older than `RECONCILE_AFTER` (2 min), it asks the gateway `GET /charges/{charge_key}`:

| Gateway says | Worker does | Code it reuses |
|---|---|---|
| succeeded | book, or (hold dead) `REFUND_PENDING` + outbox | `completeCharged` → `CompletePayment` (the confirm's own Tx B) |
| declined | `DECLINED` | `MarkPaymentDeclined` |
| not found | **void the key**, then `ABANDONED` (see section 4) | `MarkPaymentAbandoned` |
| no answer | stays `PENDING`, error recorded, retried later with backoff | `RecordReconcileFailure` |

Why "older than 2 min": the reconciler must never race a request that is still running. That's why the worker **refuses to start** unless `RECONCILE_AFTER > PAYMENT_TIMEOUT + IDEMPOTENCY_LEASE`.

### Job 2: Outbox drainer (every 5s)
**Story without it:** Bob's hold expired while he was paying, so a refund is owed. The inline refund hit a gateway error. The outbox row says "refund Bob", but **nobody ever reads it**. Bob never gets his money back.

**What it does:** claims `PENDING` outbox rows that are due, and:
- `REFUND` → `POST /refunds` with the payment's fixed `refund_key` (idempotent), then `MarkRefunded`, the same method the inline refund uses.
- `RELEASE_LOCK` → owner-checked Redis release. Redis down is a failure → retry later. A key already gone counts as success.
- On failure: backoff, and after 20 attempts a **dead letter** (section 5).

### Job 3: Hold expirer (every 10s)
**Story without it:** Carol holds a seat and walks away. Her hold stays `ACTIVE` until *someone else* happens to try that seat (Phase 2's lazy expiry), and her Redis lock blocks it for the full 310s.

**What it does:** in **one statement** (so one transaction), marks dead `ACTIVE` holds `EXPIRED` **and** writes a `RELEASE_LOCK` outbox row for each. The drainer then frees Redis promptly (`demo/abandon.sh`: freed after ~23s, before the 30s demo TTL).

**It skips holds whose payment is `PENDING`.** That user is paying; their charge may still succeed. Expiring the hold wouldn't change the payment's outcome (`CompletePayment` checks `expires_at` itself), but releasing the Redis lock would let someone else take a seat that may be about to be paid for. The same rule (`deadHoldCondition`) is used by `CreateHold` and the seat map, so all three always agree. It's bounded by **`PENDING_HOLD_MAX` (10 min)**: if the gateway is down for ages, the seat frees anyway. That's safe because the payment stays `PENDING`, and if it later turns out to have succeeded, `CompletePayment` sees the expired hold and **refunds** (`TestPendingHoldMax_SeatFreesThenLateSuccessIsRefunded`).

### Job 4: Idempotency key cleanup (every hour)
**Story without it:** one row per confirm, forever. After a year the table is huge, and nothing in it is useful anymore.

**What it does:** deletes `COMPLETED` keys older than `IDEMPOTENCY_RETENTION` (24h). **Trade-off:** after deletion, an old key is treated as **new**, so a client retrying a day later would run again. That's why retention must be longer than any realistic retry window. Phones retry within seconds or minutes, so 24h leaves a huge margin. `IN_PROGRESS` keys are never deleted.

---

## 2. `FOR UPDATE SKIP LOCKED`: many workers, no leader

Every claim looks like this ([repository_worker.go](../../internal/booking/repository_worker.go)):

```sql
UPDATE outbox SET attempts = attempts + 1, next_attempt_at = NOW() + interval '60 seconds'
WHERE id IN (
    SELECT id FROM outbox
    WHERE status = 'PENDING' AND next_attempt_at <= NOW()
    ORDER BY next_attempt_at
    LIMIT 50
    FOR UPDATE SKIP LOCKED)
RETURNING id, kind, payload, attempts
```

- `FOR UPDATE` locks the selected rows.
- `SKIP LOCKED` means "if another transaction has already locked a row, **don't wait for it, skip it**".

Two workers claiming at the same instant therefore get **different** rows, and neither blocks the other. No leader election, no ZooKeeper, no Redis lock: **Postgres row locks are the coordination.** Proof: `TestTwoWorkers_ProcessEveryRowExactlyOnce` runs two reconcilers and then two drainers concurrently on 30 rows. Every payment shows `reconcile_attempts = 1`, every refund row `attempts = 1`, and the gateway paid exactly 30 refunds.

```
worker A: SELECT … FOR UPDATE SKIP LOCKED LIMIT 4  -> rows 1,2,3,4 (locked by A)
worker B: SELECT … FOR UPDATE SKIP LOCKED LIMIT 4  -> rows 5,6,7,8 (1-4 are locked: skipped, not waited for)
```

---

## 3. Claim-then-work, with a lease

A gateway call can take seconds. Phase 3's golden rule still applies: **never hold a transaction across a network call.** So every job works in three steps:

```
1. CLAIM   (short tx, commits at once)  next_attempt_at = NOW() + 60s   <- the LEASE
                                         attempts = attempts + 1
2. WORK    (NO transaction open)         call the gateway / Redis
3. RECORD  (new short tx)                DONE, or backoff, or FAILED
```

The **lease** is the "due time" pushed into the future: while it lasts, no other worker claims the row. If the worker **crashes** between 1 and 3, nothing is lost. The lease runs out, the row is due again, and any worker claims it (`demo/worker-crash.sh`: SIGKILL mid-backlog, restart, all 12 complete exactly once).

The work is then possibly done **twice**, which is why every side effect is **idempotent**: `refund_key` (the gateway refunds once), owner-checked lock release, and `CompletePayment` (a second run sees `SUCCEEDED` and returns the same booking).

The lease must be longer than one piece of work. Otherwise a slow worker would lose its claim mid-work and a second worker would start the same row. The worker checks this at startup: `RECONCILE_LEASE > 2 × PAYMENT_TIMEOUT + DB_QUERY_TIMEOUT` (lookup + void + Tx B).

**Attempts are counted at claim time**, not at failure. A row that crashes the worker every time still burns an attempt per crash and eventually becomes a dead letter, instead of crashing workers forever.

---

## 4. The void race at reconciliation

"The gateway has no charge for `pay-7`" sounds like "no money was taken, abandon it". **But a slow request might still be on its way** with that very key: a delayed packet, or a client retrying with the old `Idempotency-Key`.

```
 t0  reconciler: GET /charges/pay-7  -> 404 not found
 t1  reconciler: mark payment ABANDONED
 t2  late request: POST /charges pay-7 -> SUCCEEDED      <- money taken, payment ABANDONED,
                                                            nobody will ever book or refund it
```

The fix is to **close the key at the gateway before giving up on it**:

```
 t0  reconciler: GET /charges/pay-7          -> 404
 t1  reconciler: POST /charges/pay-7/void    -> "voided"   (key closed)
 t2  late request: POST /charges pay-7       -> "voided"   (refused, NO money taken)
 t3  reconciler: payment ABANDONED                          (now provably safe)
```

The gateway handles "void" and "charge" for one key **one at a time**, so exactly one wins:
- **void wins** → no charge can ever happen with that key → `ABANDONED` is safe;
- **the late charge wins** (it landed just before the void) → the void answers **"succeeded"** → the reconciler takes the normal succeeded path (`TestReconciler_VoidRace_LateChargeWins_Books`).

And if the user comes back later with the old `Idempotency-Key`: the 504 wasn't stored, so the request runs again. `BeginPayment` only reuses `PENDING` payments, so it creates a **new payment with a new `charge_key`**. If a charge does hit the voided key, the API answers **504 `PAYMENT_NOT_COMPLETED`** ("No money was taken. Please retry."). That's a different code from `PAYMENT_PENDING`, because here we *know* the outcome. Total: **1 successful charge** (`TestPay_RetryWithOldKeyAfterWorkerAbandoned_ChargesOnce`).

Also: **"gateway down" is never treated as "not found".** A void happens only after a *definite* 404. An unreachable gateway leaves the payment `PENDING` with backoff.

---

## 5. Backoff, jitter and dead letters

**Backoff:** after the n-th failure, wait `min(2^n, 300)` seconds: 2s, 4s, 8s, … capped at 5 min. A broken gateway isn't hammered, and recovery is still noticed within 5 minutes. (`TestDrainer_BackoffThenSuccess` logs `2.2s, 4.6s, 8.0s`.)

**Jitter:** multiply by `1 + random() × 0.2`. Rows that failed **together** (say 500 refunds during a 1-minute gateway outage) would otherwise all retry in the **same second**, a self-made traffic spike (the "thundering herd"). Jitter spreads them out. The job tickers also have ±10% jitter, so replicas don't all hit the database at once.

All of this is computed in SQL (`NOW() + make_interval(…)`), so every worker uses the **database clock**.

**Dead letters:** after `OUTBOX_MAX_ATTEMPTS` (20) the row becomes `FAILED`, is logged at **ERROR**, and is **never retried automatically**. Why not retry forever?
- Some rows can **never** succeed (a bad payload, a charge the gateway no longer knows). Infinite retries hide the problem behind endless WARN lines.
- They waste work, and with many stuck rows, crowd out the healthy ones.
- A human needs to look. `make outbox-retry ID=<id>` re-queues the row after the cause is fixed.

The inline path (Phase 3's `RecordOutboxFailure`) now counts the attempt and sets a backoff too, so it follows the same schedule.

---

## 6. Why the worker reuses `CompletePayment` instead of copying it

"The charge succeeded; now book it, or refund it if the hold is gone, but never refund a payment that already backs a booking" is the **most delicate rule** in SeatLock. It lives in exactly one place: `CompletePayment` (Tx B), wrapped by `completeCharged` (Tx B + Redis lock release). Both `ConfirmBooking` (API) and `ReconcilePayment` (worker) call it.

If the worker had its own copy, the two would slowly **drift**: a bug fixed in one but not the other, a new status handled in one but not the other. One day the worker would book a seat the API would have refunded, or the reverse. **One rule, one implementation**: the worker only adds *scheduling* (`internal/worker`) on top of *business actions* (`booking.Service`). The only difference is deliberate: the API refunds inline because a user is waiting, while the worker leaves refunds to the outbox drainer.

---

## 7. Lessons from building Phase 4

- **The memory-pressure flake is still real.** Two `verify.sh` runs failed with `get show 1: service busy: context deadline exceeded` while the `marketpulse` cluster was busy; re-runs passed. The push gate did its job: nothing failing was pushed.
- **Demo scripts are code too.** `worker-crash.sh` first "charged" keys named `INSERT`, `0` and `12`: `psql -tA` prints the command tag after `RETURNING` rows. `psql -q` fixed it. The worker itself was correct.
- **Demo timings live in a separate Compose override** (`docker-compose.demo.yml`), and they still have to pass the worker's startup safety checks. CI and tests never use them.

---

## 8. Interview questions (with short answers)

**Q1. Your server crashed after charging the card. How does the money end up correct?**
The payment row was committed as `PENDING` *before* the charge (Tx A). If the client retries, the same `charge_key` is replayed and Tx B runs. If nobody retries, the worker's reconciler finds the `PENDING` payment after 2 minutes, asks the gateway, and runs the same Tx B: book if the hold is still valid, otherwise owe a refund via the outbox, which the drainer pays. The money always ends **booked, refunded, declined, or never taken**.

**Q2. How do two workers avoid processing the same row?**
`SELECT … FOR UPDATE SKIP LOCKED` inside the claim: each worker locks the rows it selects, and the other skips locked rows instead of waiting. The claim also sets a lease (`next_attempt_at` in the future), so the row stays invisible to others after the claim commits.

**Q3. What if a worker dies while holding a claim?**
Nothing is lost. The claim was only a lease; when it expires the row is due again and any worker picks it up. Because the work may then run twice, every side effect is idempotent.

**Q4. Why not hold the row lock while calling the gateway?**
The lock would be held for seconds; other workers and API requests touching that row would queue, holding connections, and a slow gateway would exhaust the pool. Claim, commit, call, then record.

**Q5. The gateway says "no such charge". Why not just mark the payment failed?**
A slow request with that key could still land afterwards and take money nobody would book or refund. We **void** the key first: a later charge with it is refused, so `ABANDONED` is provably safe. If the late charge got there first, the void says "succeeded" and we book.

**Q6. Why exponential backoff with jitter?**
Exponential: don't hammer a broken dependency, back off fast. Cap: still notice recovery quickly. Jitter: rows that failed together shouldn't all retry in the same second (thundering herd).

**Q7. What is a dead letter and why have one?**
A row that failed N times is parked as `FAILED`, logged at ERROR, and not retried automatically. Infinite retries hide permanently broken work and waste capacity; a human should look and re-queue it.

**Q8. Why does the hold expirer skip holds that have a pending payment?**
The user may already have paid. Releasing the seat (and its Redis lock) would hand a possibly-paid seat to someone else. The skip is bounded by `PENDING_HOLD_MAX`; after that the seat frees, and a late success is refunded by the same `CompletePayment` rule.

**Q9. Why does the worker reuse the API's `CompletePayment` instead of its own logic?**
One rule, one implementation: two copies of "book or refund, never refund a booked payment" would drift and eventually disagree, which means a double booking or a wrong refund.

**Q10. Why must idempotency-key retention be longer than the client retry window?**
After a key is deleted, a request with it is treated as new and runs again. If clients could still retry after deletion, a retry would act like a fresh request. Retention (24h) is far longer than real retry windows (seconds to minutes).

**Bonus. How do you know the worker's timings are safe?**
It validates them at startup and refuses to run otherwise, for example `RECONCILE_AFTER > PAYMENT_TIMEOUT + IDEMPOTENCY_LEASE`, so it never touches a payment whose request may still be running.

---

## 9. What changes in Phase 5

Phase 5 makes all of this **visible**. The API and the worker expose **Prometheus metrics** on `/metrics`:
- request rates, latencies and status codes per endpoint;
- holds and bookings created, 409/503 rates;
- payments by outcome;
- the worker's per-job **claimed / succeeded / failed** counts (already returned by every `RunOnce`);
- outbox backlog, dead letters, the age of the oldest `PENDING` payment and refund.

**Grafana** dashboards show them live, and alerts fire on what needs a human: dead letters, a growing outbox, `PENDING` payments older than a few minutes, `LOCK_UNAVAILABLE` spikes.
