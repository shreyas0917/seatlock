# SeatLock design reference

The details behind the [README](../README.md): the request flow, how each guarantee is enforced, the worker, the API, and how observability is built. The reasoning behind each decision is in the [ADRs](adr/README.md); the step-by-step story is in the [learning notes](learning/).

## Contents
- [Request flow](#request-flow)
- [How the guarantees are enforced](#how-the-guarantees-are-enforced)
- [The background worker](#the-background-worker)
- [API reference](#api-reference)
- [Observability internals](#observability-internals)
- [Project layout](#project-layout)

## Request flow

```
client ──HTTP──> SeatLock API (Go, chi) ───────────────┬──> Redis           fast seat lock (SET NX EX 310), holds only
                  │  hold   : Redis lock, then Postgres  ├──> PostgreSQL      holds, payments, bookings,
                  │  confirm: Tx A -> gateway -> Tx B    │                    idempotency keys, outbox  (the authority)
                  │  cancel : Postgres + outbox          └──> Payment gateway (mock, cmd/payment)
                  │
worker (cmd/worker, N replicas, no leader) ── claims rows with FOR UPDATE SKIP LOCKED, then:
                  ├─ payment reconciler  : abandoned PENDING payments -> booked / refunded / declined / abandoned
                  ├─ outbox drainer      : REFUND + RELEASE_LOCK rows -> DONE (backoff, dead letters)
                  ├─ hold expirer        : dead ACTIVE holds -> EXPIRED + RELEASE_LOCK row
                  ├─ invariants          : every verify.sql check as a live gauge, every 15s
                  └─ key cleanup         : COMPLETED idempotency keys older than 24h -> deleted

prometheus ── scrapes api:9091, worker:9092, payment (internal ports only) every 5s, evaluates alerts.yml
grafana    ── "SeatLock — Reliability" dashboard, provisioned from monitoring/grafana (no clicks)
```

Booking is two steps: **hold** a seat (5 minutes), then **confirm** it, which **pays** and books.

1. **Hold** (`POST /v1/holds`): take the Redis lock `seat:{show}:{seat}` (`SET NX EX 310`), then create the hold in Postgres inside a transaction that locks the seat row. Postgres decides; Redis only turns most race losers away in ~1 ms. If Redis can't be asked, the hold is refused (`503 LOCK_UNAVAILABLE`): **fail closed**.
2. **Confirm** (`POST /v1/bookings` + `Idempotency-Key`): claim the key (Postgres, with a lease), **Tx A** creates or reuses one `PENDING` payment for the hold, the gateway is called **outside any transaction**, **Tx B** records the outcome and creates the booking, or queues a refund in the outbox if the hold died meanwhile. Nothing here touches Redis.
3. **Repair**: whatever the request path leaves behind (a lost gateway answer, a refund that failed, an expired hold, a lock to release) is finished by the worker, using the same service code.

## How the guarantees are enforced

**Never sold twice** (three layers; only the last one is a guarantee)
1. **Redis lock**: `SET seat:{show}:{seat} <user> NX EX 310`. Turns away race losers in ~1 ms. An optimisation: Redis can lose keys.
2. **PostgreSQL hold transaction**: `SELECT … FOR UPDATE` on the seat, plus the partial unique index `holds_one_active_per_seat` (one ACTIVE hold per seat). It rejects user B even if Redis lost user A's lock.
3. **PostgreSQL booking constraints**: `UNIQUE (show_id, seat_id)` and `UNIQUE (hold_id)` on `bookings`. Whatever any code path does, the database refuses a second booking for a seat.

**Never charged twice**
1. **Idempotency keys in PostgreSQL** (lease + lease token): duplicate requests replay the stored response byte for byte.
2. **One live payment per hold** (partial unique index `payments_one_live_per_hold`): concurrent confirms, even with different keys, share one `PENDING` payment.
3. **One `charge_key` per payment** (`pay-<id>`): the gateway charges a key once and replays its result. A key the worker abandons is **voided** at the gateway first, so a late request can never use it.
4. **Tx B (`CompletePayment`) never refunds a payment that already backs a booking.**

**Never charged without a seat, never stuck**
- No transaction is open during a gateway call: **Tx A → gateway → Tx B**.
- A timeout is **not** a failure: the payment stays `PENDING` (504). A retry, or else the **worker's reconciler**, asks the gateway what really happened and books, refunds, declines or abandons.
- Refunds owed are written to the **outbox in the same transaction** as the decision; the worker retries them with backoff until `DONE`, or dead-letters them (`FAILED`) for a human.
- Every expiry decision uses the database clock (`NOW()`), never an app server's clock.

Full explanations: [phase-1.md](learning/phase-1.md) · [phase-2.md](learning/phase-2.md) · [phase-3.md](learning/phase-3.md) · [phase-4.md](learning/phase-4.md)

## The background worker

`cmd/worker` runs five jobs, each on its own jittered ticker, each claiming one batch (`WORKER_BATCH_SIZE`, default 50) per run:

| Job | Every | Repairs | Without it |
|---|---|---|---|
| **Payment reconciler** | 30s | `PENDING` payments older than `RECONCILE_AFTER` (2m): asks the gateway, then books (same `CompletePayment` as confirm), queues a refund, declines, or **voids the key and abandons** | A 504 nobody retried stays `PENDING` forever: maybe paid, no seat, no refund |
| **Outbox drainer** | 5s | `REFUND` and `RELEASE_LOCK` rows: idempotent refund / owner-checked Redis release; backoff `min(2^n, 300)s` + jitter; `FAILED` after `OUTBOX_MAX_ATTEMPTS` (20) | A failed refund is never paid; locks linger until their TTL |
| **Hold expirer** | 10s | `ACTIVE` holds past expiry → `EXPIRED` + `RELEASE_LOCK` (one transaction). Skips holds with a `PENDING` payment until `PENDING_HOLD_MAX` (10m) | Expired holds stay `ACTIVE` until someone else tries the seat |
| **Invariants** | 15s | Runs every `verify.sql` check under a statement timeout and exports `seatlock_invariant_violations{check}` + outbox/payment/hold gauges | A double booking is noticed only if someone runs SQL by hand |
| **Key cleanup** | 1h | `COMPLETED` idempotency keys older than `IDEMPOTENCY_RETENTION` (24h) | The table grows forever |

- **Run as many replicas as you like**, no leader election: `docker compose up -d --scale worker=2`. Rows are claimed with `FOR UPDATE SKIP LOCKED` plus a lease.
- **Crash-safe**: a killed worker's claims expire and are picked up again; every side effect is idempotent.
- **Refuses to start with unsafe timings** (e.g. `RECONCILE_AFTER` must be > `PAYMENT_TIMEOUT + IDEMPOTENCY_LEASE`).
- **Dead letters**: `make outbox-retry ID=<id>` re-queues one after you fixed its cause.

## API reference

Interactive docs: http://localhost:8080/swagger/index.html (generated from the handler annotations, `make swagger`).

Headers: `X-User-ID` identifies the user (there is no auth in this project) and is required on every write. `POST /v1/bookings` also requires **`Idempotency-Key`** (1–64 characters, one per logical "Pay" action).

| Method | Path | Description | Responses |
|---|---|---|---|
| GET | `/healthz` | Process is alive | 200 |
| GET | `/readyz` | `{"postgres":"up","redis":"up"}`. 503 **only** if Postgres is down | 200, 503 |
| GET | `/v1/shows` | List shows | 200 |
| GET | `/v1/shows/{showID}/seats` | Seat map: `AVAILABLE` / `HELD` / `BOOKED`, from Postgres only (a seat whose payment is in flight stays `HELD`) | 200, 400, 404 |
| POST | `/v1/holds` | Hold a seat: `{"show_id":1,"seat_id":7}` → `hold_id`, `expires_at`, `seconds_left` | **201** new, **200** same user's existing hold, 400, 404, 409, **503** |
| DELETE | `/v1/holds/{holdID}` | Cancel your hold (refused while a payment is in flight) | 200, 400, 404, 409, 410 |
| POST | `/v1/bookings` | **Confirm = pay + book**: `{"hold_id":12}` + `Idempotency-Key` | **201**, 400, **402**, 404, 409, **410**, **422**, 503, **504** |
| GET | `/v1/bookings/{bookingID}` | Get one booking | 200, 400, 404 |

```bash
curl localhost:8080/v1/shows/1/seats
curl -X POST localhost:8080/v1/holds    -H "X-User-ID: alice" -d '{"show_id":1,"seat_id":7}'   # -> hold_id
curl -X POST localhost:8080/v1/bookings -H "X-User-ID: alice" -H "Idempotency-Key: pay-7-try-1" -d '{"hold_id":1}'
```

Sending the same confirm again with the same `Idempotency-Key` returns the **stored response, byte for byte**, with header `Idempotent-Replayed: true`, and never charges again.

All errors share one shape:

```json
{ "error": { "code": "HOLD_EXPIRED", "message": "this hold has expired or was cancelled", "payment": "REFUNDED" } }
```

| Code | HTTP | Meaning |
|---|---|---|
| `INVALID_INPUT` / `MISSING_USER_ID` / `SEAT_NOT_IN_SHOW` | 400 | Bad request |
| `MISSING_IDEMPOTENCY_KEY` | 400 | Confirm without a valid `Idempotency-Key` |
| `PAYMENT_DECLINED` | 402 | Card declined, nothing charged, **hold kept**: retry with a NEW key |
| `SHOW_NOT_FOUND` / `SEAT_NOT_FOUND` / `BOOKING_NOT_FOUND` | 404 | Unknown id |
| `HOLD_NOT_FOUND` | 404 | No such hold, **or someone else's** (not revealed) |
| `SEAT_HELD` | 409 | Another user holds the seat |
| `SEAT_ALREADY_BOOKED` | 409 | The seat is sold |
| `HOLD_ALREADY_USED` | 409 | The hold was already confirmed |
| `PAYMENT_IN_PROGRESS` | 409 | A payment for this hold is in flight: cannot cancel it, and re-holding the seat is refused |
| `REQUEST_IN_PROGRESS` | 409 | A request with this `Idempotency-Key` is still running |
| `HOLD_EXPIRED` | 410 | The hold expired or was cancelled; `error.payment` says `REFUNDED` / `REFUND_PENDING` if money had been taken |
| `IDEMPOTENCY_KEY_REUSED` | 422 | Same key, different request |
| `LOCK_UNAVAILABLE` | 503 | Redis unreachable: new holds **fail closed** |
| `SERVICE_BUSY` | 503 | Database timeout, pool exhausted, or Postgres briefly unavailable (temporary, `Retry-After`) |
| `PAYMENT_PENDING` | 504 | Payment outcome unknown (lost answer): retry with the **same** key |
| `PAYMENT_NOT_COMPLETED` | 504 | The old attempt was abandoned by the worker; **no money was taken**. Please retry |
| `INTERNAL_ERROR` | 500 | Unexpected error (details only in logs) |

## Observability internals

`make up` starts **Prometheus** and **Grafana** with everything provisioned from the repo; the **"SeatLock — Reliability"** dashboard is Grafana's home page (http://localhost:3001, no login).

| Dashboard row | What it shows |
|---|---|
| **1 · Correctness** (top, large) | Double bookings and **every `verify.sql` invariant as a live gauge** (green at 0, red otherwise), outbox dead letters, oldest `PENDING` payment, invariant check errors and freshness |
| **2 · Traffic and latency** | Requests/s by route pattern and status; p50/p95/p99 for holds and bookings |
| **3 · Booking outcomes** | Holds by outcome (incl. `lock_unavailable` = fail-closed), confirms by outcome, idempotency results |
| **4 · Dependencies** | Redis lock ops (`error` = fail-closed), Redis and DB pools, DB acquire wait, gateway results and p95 |
| **5 · Worker** | Job runs and duration, rows by outcome, seconds since last success, outbox backlog, money taken vs refunds at the gateway |
| **6 · Alerts** | Alerts firing right now |

- **One metrics package** ([internal/metrics](../internal/metrics/metrics.go)): every metric documented with its labels and the panel or alert that uses it.
- **Low cardinality**: no user, seat, hold or key ids in labels; HTTP `route` is the chi pattern (`/v1/holds/{holdID}`), never the raw path; unknown methods become `OTHER`. No label is called `job` or `instance` (worker jobs use `worker_job`). A test enforces it.
- **`/metrics` and `/debug/pprof` only on internal ports** (api `:9091`, worker `:9092`), not published to the host; the public API answers both with 404 (tested).
- **Invariants never show a false green**: a check that fails or times out keeps its last value and counts an error.
- The alerts, what each means and what to do: [RUNBOOK.md](RUNBOOK.md).

## Project layout

```
cmd/api/              HTTP server entry point (wiring, graceful shutdown)
cmd/worker/           background worker entry point (5 jobs, graceful shutdown)
cmd/payment/          mock payment gateway binary
cmd/migrate/          `make migrate-up` / `make migrate-down`
internal/config/      environment variables + the worker's safety checks
internal/db/          pgx pool + migration runner
internal/booking/     domain: models, errors, services (holds, payments, worker actions),
                      repositories (SQL, incl. SKIP LOCKED claims), SeatLocker and PaymentGateway interfaces
internal/worker/      job runner (tickers, jitter, shutdown, metrics) and the five jobs
internal/metrics/     every Prometheus metric, in one place; pool collectors; the internal metrics/pprof server
internal/invariants/  the invariant checks (single source of demo/verify.sql and the live gauges)
internal/lock/        Redis SeatLocker (SET NX EX, Lua release)
internal/payment/     HTTP PaymentGateway client (charge, refund, lookup, void)
internal/paymentmock/ the mock gateway (idempotent charges/refunds, void, forced behaviours)
internal/idempotency/ Idempotency-Key store (Postgres, leases, cleanup)
internal/httpapi/     handlers, router, JSON helpers, error -> HTTP status mapping
db/migrations/        SQL schema (embedded into the binary)
db/seed/              demo data (seed.sql) and load-test data (load.sql)
demo/                 demo scripts + verify.sql
loadtest/             k6 scripts (rush, capacity, spike, soak, smoke), run.sh, capacity.sh, report tool
chaos/                chaos scripts (Redis, api replica, worker, slow gateway, Postgres) run under load
monitoring/           prometheus.yml, alerts.yml (+ promtool tests), Grafana provisioning and dashboard
deploy/lb/            nginx config of the `scale` profile
scripts/verify.sh     the push gate (also run by CI)
docs/                 this file, ADRs, runbook, results, learning notes, Swagger output, images
docker-compose.demo.yml   short timings for the worker demos (`make up-demo`)
docker-compose.bench.yml  realistic 50-150 ms gateway for the load tests (`make up-bench`)
```
