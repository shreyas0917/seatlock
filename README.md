# SeatLock

[![CI](https://github.com/shreyas0917/seatlock/actions/workflows/ci.yml/badge.svg)](https://github.com/shreyas0917/seatlock/actions/workflows/ci.yml)

**A high-concurrency ticket booking backend in Go that guarantees a seat can never be sold twice, a card is never charged twice, and money taken for an expired hold is refunded, even when 50 users click at once and even when Redis fails.**

SeatLock is a backend-only system design project (think BookMyShow). It follows a **fail-closed booking policy**:

> Cache unavailability must never result in unauthorized bookings. Existing valid reservations may complete using authoritative database state.

- **Redis** = a fast, temporary seat lock. An optimisation, never the source of truth.
- **PostgreSQL** = the authority for holds, hold expiry, payments, **idempotency keys** and bookings. Redis can never override a failed PostgreSQL check, every expiry decision uses the database clock (`NOW()`), and **nothing in the confirm/payment path depends on Redis**.

## Tech stack

Go · chi · PostgreSQL 16 (pgx, hand-written SQL) · Redis 7 (go-redis) · golang-migrate · slog · Swagger (swag) · Docker Compose · GitHub Actions

## Quick start

Requirements: Go 1.22+, Docker, make, bash (Git Bash on Windows).

```bash
cp .env.example .env    # local settings (Postgres on host port 5434, mock gateway on 8095)
make up                 # build + start postgres, redis, the mock payment gateway and the api
make migrate-up         # create tables (the api also does this on startup)
make seed               # 1 show, 100 seats (safe to run twice)
make run                # optional: run the api from source instead of Docker (stop the api container first)
```

API: http://localhost:8080 · Swagger UI: http://localhost:8080/swagger/index.html · Mock gateway: http://localhost:8095

Booking is two steps: **hold** a seat (5 minutes), then **confirm** it, which **pays** and books.

```bash
curl localhost:8080/v1/shows/1/seats
curl -X POST localhost:8080/v1/holds    -H "X-User-ID: alice" -d '{"show_id":1,"seat_id":7}'   # -> hold_id
curl -X POST localhost:8080/v1/bookings -H "X-User-ID: alice" -H "Idempotency-Key: pay-7-try-1" -d '{"hold_id":1}'
```

## Architecture

```
client ──HTTP──> SeatLock API (Go, chi) ──────────────┬──> Redis        fast seat lock (SET NX EX 310), holds only
                  │  hold   : Redis lock, then Postgres ├──> PostgreSQL   holds, payments, bookings,
                  │  confirm: Tx A -> gateway -> Tx B   │                 idempotency keys, outbox  (the authority)
                  │  cancel : Postgres + outbox         └──> Payment gateway (mock, cmd/payment)
```

![SeatLock architecture](docs/architecture.png)

*(The diagram shows the target design across all phases; the waiting room, worker, metrics and load tests come later.)*

## API

Headers: `X-User-ID` identifies the user (there is no auth in this project) and is required on every write. `POST /v1/bookings` also requires **`Idempotency-Key`** (1–64 characters, one per logical "Pay" action).

| Method | Path | Description | Responses |
|---|---|---|---|
| GET | `/healthz` | Process is alive | 200 |
| GET | `/readyz` | `{"postgres":"up","redis":"up"}`. 503 **only** if Postgres is down | 200, 503 |
| GET | `/v1/shows` | List shows | 200 |
| GET | `/v1/shows/{showID}/seats` | Seat map: `AVAILABLE` / `HELD` / `BOOKED`, from Postgres only | 200, 400, 404 |
| POST | `/v1/holds` | Hold a seat: `{"show_id":1,"seat_id":7}` → `hold_id`, `expires_at`, `seconds_left` | **201** new, **200** same user's existing hold, 400, 404, 409, **503** |
| DELETE | `/v1/holds/{holdID}` | Cancel your hold (refused while a payment is in flight) | 200, 400, 404, 409, 410 |
| POST | `/v1/bookings` | **Confirm = pay + book**: `{"hold_id":12}` + `Idempotency-Key` | **201**, 400, **402**, 404, 409, **410**, **422**, 503, **504** |
| GET | `/v1/bookings/{bookingID}` | Get one booking | 200, 400, 404 |

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
| `PAYMENT_IN_PROGRESS` | 409 | Cannot cancel a hold while its payment is in flight |
| `REQUEST_IN_PROGRESS` | 409 | A request with this `Idempotency-Key` is still running |
| `HOLD_EXPIRED` | 410 | The hold expired or was cancelled; `error.payment` says `REFUNDED` / `REFUND_PENDING` if money had been taken |
| `IDEMPOTENCY_KEY_REUSED` | 422 | Same key, different request |
| `LOCK_UNAVAILABLE` | 503 | Redis unreachable: new holds **fail closed** |
| `SERVICE_BUSY` | 503 | Database timeout / pool exhausted (temporary, `Retry-After`) |
| `PAYMENT_PENDING` | 504 | Payment outcome unknown (lost answer): retry with the **same** key |
| `INTERNAL_ERROR` | 500 | Unexpected error (details only in logs) |

## How the guarantees are enforced

**Never sold twice** (three layers)
1. **Redis lock**: `SET seat:{show}:{seat} <user> NX EX 310`. Turns away race losers in ~1ms.
2. **PostgreSQL hold transaction**: `SELECT … FOR UPDATE` on the seat, plus the partial unique index `holds_one_active_per_seat`. The authority: it rejects user B even if Redis lost user A's lock.
3. **PostgreSQL booking constraints**: `UNIQUE (show_id, seat_id)` and `UNIQUE (hold_id)` on `bookings`.

**Never charged twice**
1. **Idempotency keys in PostgreSQL** (`idempotency_keys`, with a lease + lease token): duplicate requests replay the stored response.
2. **One live payment per hold** (partial unique index `payments_one_live_per_hold`): concurrent confirms, even with different keys, share one `PENDING` payment.
3. **One `charge_key` per payment** (`pay-<id>`): the gateway charges a key once and replays its result.
4. **Tx B never refunds a payment that already backs a booking.**

**Never charged without a seat**
- No transaction is open during the gateway call: **Tx A → gateway → Tx B**.
- A timeout is **not** a failure: the payment stays `PENDING` (504), and a retry with the same key learns the real outcome.
- If the hold expired during payment, Tx B writes `REFUND_PENDING` **and an outbox `REFUND` row in the same transaction**, then refunds inline (410 + `"payment":"REFUNDED"`).

Full explanations: [phase-1.md](docs/learning/phase-1.md) · [phase-2.md](docs/learning/phase-2.md) · [phase-3.md](docs/learning/phase-3.md)

## Tests

```bash
make test               # unit + handler tests (no database, Redis or gateway)
make test-integration   # + real Postgres, Redis and the in-process mock gateway (needs `make up`)
make verify             # gofmt + vet + unit + integration tests; full log in tmp/. Push only if this passes.
```

CI (GitHub Actions) runs the same `scripts/verify.sh` **with `-race`** against Postgres 16 and Redis 7 service containers.

Key proofs (integration):

| Test | Proves |
|---|---|
| `TestHoldRace` | 50 users, same seat → **1 hold, 49 `ErrSeatHeld`** |
| `TestConcurrentBookingSameSeat` | 50 raw booking inserts for one seat → the UNIQUE constraint lets exactly 1 through |
| `TestRedisLostData_PostgresWins` | Redis `FLUSHDB` → B gets the Redis lock but **Postgres rejects B** |
| `TestPay_SameKeyThreeTimes_ReplaysIdentically` | same key 3× → identical responses, **1 booking, 1 charge** |
| `TestPay_SameKeyConcurrent_OneChargeOneBooking` | same key, 10 concurrent → **1 charge, 1 booking**, the rest 201/409 `REQUEST_IN_PROGRESS` |
| `TestPay_TimeoutThenRetrySameKey_OneCharge` | lost answer → 504, payment `PENDING`; retry → 201, **still 1 charge** |
| `TestPay_HoldExpiresDuringPayment_Refunded` | hold expires mid-payment → **410, refunded, no booking** |
| `TestPay_TwoKeysSameHoldConcurrent_OneBookingNoRefund` | two keys, one hold, concurrent → **1 booking, 1 charge, 0 refunds** |
| `TestPay_RedisDown_ConfirmStillWorks` | Redis unreachable → new holds 503, **confirm with payment 201** |
| `TestPay_NoTransactionOpenDuringGatewayCall` | slow gateway → rows lockable (`NOWAIT`), no `idle in transaction`, cancel → 409 |

Integration tests use the `seatlock_test` database and Redis database 1, so they never touch your demo data. **Always run them through `make test-integration` / `make verify`** (they pass `-p 1`; packages share the test database).

## Demos (against the running stack)

```bash
make reset-data                      # free all seats (keeps shows/seats; clears Redis seat locks too)
./demo/race.sh                       # 20 users race to hold one seat -> 201: 1, 409: 19; winner pays -> 201
./demo/redis-down.sh                 # Redis stopped: new hold 503, existing hold pays + books 201, recovery
./demo/duplicate-request.sh          # same confirm + same key 5x -> 1 booking, 1 charge, 4 identical replays
./demo/payment-timeout.sh            # money taken, answer lost -> 504 -> retry same key -> 201, still 1 charge
./demo/expired-during-payment.sh     # hold expires mid-payment -> 410, refunded, no booking
```

Every demo resets the mock gateway first, prints PASS/FAIL per step and the gateway's stats, exits non-zero on any failure, and ends with [demo/verify.sql](demo/verify.sql): 7 invariant checks that must all return 0 rows (no double booking, every booking backed by exactly one `SUCCEEDED` payment, no `SUCCEEDED` payment without a booking, at most one live payment per hold, no refunded payment behind a booking, …).

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `HTTP_ADDR` | `:8080` | Listen address |
| `DATABASE_URL` | `postgres://…@localhost:5434/seatlock` | Postgres |
| `RUN_MIGRATIONS` | `true` | Apply migrations on startup |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | `25` / `5` | Postgres pool size |
| `DB_QUERY_TIMEOUT` | `3s` | One query/transaction, including waiting for a pool connection |
| `REDIS_ADDR` | `localhost:6379` | Redis |
| `REDIS_DIAL_TIMEOUT` / `_READ_` / `_WRITE_` | `200ms` | Short on purpose: fail closed fast |
| `REDIS_POOL_SIZE` / `REDIS_MIN_IDLE_CONNS` | `10` / `10` | Small warm pool |
| `HOLD_TTL` | `300s` | Hold length in Postgres |
| `REDIS_LOCK_EXTRA` | `10s` | Redis lock lives `HOLD_TTL + extra` = 310s |
| `PAYMENT_URL` | `http://localhost:8095` | Payment gateway |
| `PAYMENT_TIMEOUT` | `5s` | One gateway call; on timeout the outcome is unknown (504) |
| `IDEMPOTENCY_LEASE` | `30s` | How long a request owns an `Idempotency-Key` before a retry may take it over |
| `PAYMENT_PORT` / `PAYMENT_FAIL_RATE` / `PAYMENT_MIN_DELAY` / `PAYMENT_MAX_DELAY` / `PAYMENT_SLOW_DELAY` | `8095` / `0` / `0s` / `0s` / `3s` | Mock gateway (demos) |

## Known limitations (to be addressed in later phases)

- **No background worker yet (Phase 4).** Abandoned `PENDING` payments (a 504 never retried) are not reconciled, `PENDING` outbox rows (failed inline refunds, Redis lock releases while Redis was down) are not drained, and expired holds are only marked `EXPIRED` lazily.
- **Hold hoarding.** A user can re-hold the same seat right after their hold expires, indefinitely. A later phase will add a per-user cooldown or limit.
- **10-second Redis/DB window.** For up to 10s after a hold expires, the seat map shows `AVAILABLE` but a new hold by another user can still get `409 SEAT_HELD` from Redis (Redis lock 310s vs DB hold 300s, on purpose).
- **A confirm with a new key after the booking exists** answers `409 HOLD_ALREADY_USED` (only a confirm that overlaps the in-flight payment gets the same booking back).
- **Idempotency keys are never deleted yet.** A retention/cleanup job comes with the worker.

## Project layout

```
cmd/api/              HTTP server entry point (wiring, graceful shutdown)
cmd/payment/          mock payment gateway binary
cmd/migrate/          `make migrate-up` / `make migrate-down`
internal/config/      environment variables
internal/db/          pgx pool + migration runner
internal/booking/     domain: models, errors, services (holds, payments), repositories (SQL),
                      SeatLocker and PaymentGateway interfaces
internal/lock/        Redis SeatLocker (SET NX EX, Lua release)
internal/payment/     HTTP PaymentGateway client
internal/paymentmock/ the mock gateway (idempotent charges/refunds, forced behaviours)
internal/idempotency/ Idempotency-Key store (Postgres, leases)
internal/httpapi/     handlers, router, JSON helpers, error -> HTTP status mapping
db/migrations/        SQL schema (embedded into the binary)
db/seed/              demo data
docs/                 Swagger output, architecture diagram, learning notes
demo/                 demo scripts + verify.sql
scripts/verify.sh     the push gate (also run by CI)
```

## Make targets

`up` · `down` · `logs` · `run` · `test` · `test-integration` · `verify` · `migrate-up` · `migrate-down` · `seed` · `reset-data` · `swagger` · `lint`

`make swagger` needs the swag CLI once: `go install github.com/swaggo/swag/cmd/swag@v1.16.4`

## Phase status

- [x] Phase 0: Project setup (Docker Compose, Makefile, config)
- [x] Phase 1: Basic booking API with UNIQUE-constraint safety net + concurrency proof
- [x] Phase 2: Redis seat holds (`SET NX EX 310`), PostgreSQL as authority for holds, fail-closed
- [x] Phase 3: Payments (mock gateway), idempotency keys in PostgreSQL, refund on expiry, outbox, CI
- [ ] Phase 4: Background worker: expire holds, reconcile PENDING payments, drain the outbox
- [ ] Phase 5: Prometheus + Grafana
- [ ] Phase 6: k6 load tests
- [ ] Phase 7: Chaos scripts, full CI
- [ ] Phase 8: To be defined
