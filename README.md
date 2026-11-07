# SeatLock

[![CI](https://github.com/shreyas0917/seatlock/actions/workflows/ci.yml/badge.svg)](https://github.com/shreyas0917/seatlock/actions/workflows/ci.yml)

**A high-concurrency ticket booking backend in Go that guarantees a seat can never be sold twice, even when 50 users click "Book" at the same instant, and even when Redis fails.**

SeatLock is a backend-only system design project (think BookMyShow). It follows a **fail-closed booking policy**:

> Cache unavailability must never result in unauthorized bookings. Existing valid reservations may complete using authoritative database state.

- **Redis** = a fast, temporary seat lock. An optimisation, never the source of truth.
- **PostgreSQL** = the authority for holds, hold expiry and bookings. Redis can never override a failed PostgreSQL check, and every expiry decision uses the database clock (`NOW()`).

## Tech stack

Go · chi · PostgreSQL 16 (pgx, hand-written SQL) · Redis 7 (go-redis) · golang-migrate · slog · Swagger (swag) · Docker Compose

## Quick start

Requirements: Go 1.22+, Docker, make, bash (Git Bash on Windows).

```bash
cp .env.example .env    # local settings (Postgres is exposed on host port 5434)
make up                 # build + start postgres, redis and the api (waits until healthy)
make migrate-up         # create tables (the api also does this on startup)
make seed               # 1 show, 100 seats (safe to run twice)
make run                # optional: run the api from source instead of Docker (stop the api container first)
```

API: http://localhost:8080. Swagger UI: http://localhost:8080/swagger/index.html

Booking is two steps: **hold** a seat (5 minutes), then **confirm** the hold.

```bash
curl localhost:8080/v1/shows/1/seats
curl -X POST localhost:8080/v1/holds    -H "X-User-ID: alice" -d '{"show_id":1,"seat_id":7}'   # -> hold_id
curl -X POST localhost:8080/v1/bookings -H "X-User-ID: alice" -d '{"hold_id":1}'
```

## API

User identity comes from the `X-User-ID` header (there is no auth in this project). It is required on every write endpoint.

| Method | Path | Description | Responses |
|---|---|---|---|
| GET | `/healthz` | Process is alive | 200 |
| GET | `/readyz` | `{"postgres":"up","redis":"up"}`. 503 **only** if Postgres is down; Redis down still gives 200 | 200, 503 |
| GET | `/v1/shows` | List shows | 200 |
| GET | `/v1/shows/{showID}/seats` | Seat map: `label`, `price_paise`, `status` (`AVAILABLE` / `HELD` / `BOOKED`), from Postgres only | 200, 400, 404 |
| POST | `/v1/holds` | Hold a seat: `{"show_id":1,"seat_id":7}` → `hold_id`, `expires_at`, `seconds_left` | **201** new, **200** same user's existing hold, 400, 404, **409**, **503** |
| DELETE | `/v1/holds/{holdID}` | Cancel your hold | 200, 400, 404, 409, 410 |
| POST | `/v1/bookings` | Confirm a hold: `{"hold_id":12}` (Postgres only, works with Redis down) | 201, 400, 404, 409, **410** |
| GET | `/v1/bookings/{bookingID}` | Get one booking | 200, 400, 404 |

All errors share one shape:

```json
{ "error": { "code": "SEAT_HELD", "message": "this seat is currently held by another user" } }
```

| Code | HTTP | Meaning |
|---|---|---|
| `INVALID_INPUT` / `MISSING_USER_ID` / `SEAT_NOT_IN_SHOW` | 400 | Bad request |
| `SHOW_NOT_FOUND` / `SEAT_NOT_FOUND` / `BOOKING_NOT_FOUND` | 404 | Unknown id |
| `HOLD_NOT_FOUND` | 404 | No such hold, **or someone else's** (not revealed) |
| `SEAT_HELD` | 409 | Another user holds the seat |
| `SEAT_ALREADY_BOOKED` | 409 | The seat is sold |
| `HOLD_ALREADY_USED` | 409 | The hold was already confirmed |
| `HOLD_EXPIRED` | 410 | The hold expired or was cancelled |
| `LOCK_UNAVAILABLE` | 503 | Redis is unreachable: new holds **fail closed** |
| `SERVICE_BUSY` | 503 | Database timeout / pool exhausted (temporary, has `Retry-After`) |
| `INTERNAL_ERROR` | 500 | Unexpected error (details only in logs) |

## How double booking is prevented (three layers)

1. **Redis lock**: `SET seat:{show}:{seat} <user> NX EX 310`. Turns away the 49 losers of a race in about 1ms.
2. **PostgreSQL hold transaction**: `SELECT … FOR UPDATE` on the seat, then the partial unique index `holds_one_active_per_seat` (at most one ACTIVE hold per seat). This is the authority, and it rejects user B even if Redis lost user A's lock.
3. **PostgreSQL booking constraints**: `UNIQUE (show_id, seat_id)` and `UNIQUE (hold_id)` on `bookings`. The final safety net: of two concurrent inserts, exactly one commits, and the other gets `23505` → 409.

Full explanations: [docs/learning/phase-1.md](docs/learning/phase-1.md), [docs/learning/phase-2.md](docs/learning/phase-2.md).

## Tests

```bash
make test               # unit + handler tests (no database, no Redis)
make test-integration   # + real Postgres and Redis (needs `make up`)
make lint               # go vet + gofmt check
make verify             # gofmt + vet + unit + integration tests; full log in tmp/ (push only if this passes)
```

Key proofs (integration):

| Test | Proves |
|---|---|
| `TestHoldRace` | 50 users, same seat → **1 hold, 49 `ErrSeatHeld`**, 1 ACTIVE row |
| `TestConfirmRace` | 10 confirms of one hold → **1 booking, 9 `ErrHoldAlreadyUsed`** |
| `TestRedisLostData_PostgresWins` | Redis `FLUSHDB` while A holds → B gets the Redis lock but **Postgres rejects B (409)** |
| `TestRedisDown_FailClosed_ExistingHoldStillConfirms` | Redis unreachable → new hold `ErrLockUnavailable`, **no row written**; A's existing hold **confirms** |
| `TestConcurrentBookingSameSeat` | 50 raw booking inserts for one seat → the UNIQUE constraint lets exactly 1 through |
| `TestHTTP_ParallelHoldBurstHasNo500s` | 50 parallel HTTP holds with default pool settings → only 201/409, **never 500** |

Integration tests use the `seatlock_test` Postgres database and Redis database 1, so they never touch your seeded data. **Always run them through `make test-integration`.** It passes `-p 1`, because packages share the test database and must not truncate it in parallel. Running `go test -tags=integration ./...` without `-p 1` gives false failures.

## Demos (against the running stack)

```bash
./demo/race.sh         # 20 users race to hold one seat -> 201: 1, 409: 19, then the winner confirms -> 201
./demo/redis-down.sh   # stops Redis: new hold -> 503, existing hold confirms -> 201, recovery -> 201
```

Both end by running [demo/verify.sql](demo/verify.sql). Every check must return 0 rows: no double bookings, no booking without a matching CONSUMED hold, no seat with two ACTIVE holds. Each script prints PASS/FAIL per step and exits non-zero on any failure. `redis-down.sh` always restarts Redis, even if it fails halfway.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `HTTP_ADDR` | `:8080` | Listen address |
| `DATABASE_URL` | `postgres://…@localhost:5434/seatlock` | Postgres |
| `RUN_MIGRATIONS` | `true` | Apply migrations on startup |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | `25` / `5` | Postgres pool size |
| `REDIS_ADDR` | `localhost:6379` | Redis |
| `REDIS_DIAL_TIMEOUT` / `_READ_` / `_WRITE_` | `200ms` | Short on purpose: fail closed fast |
| `REDIS_POOL_SIZE` / `REDIS_MIN_IDLE_CONNS` | `10` / `10` | Small warm pool, so bursts do not open dozens of new connections |
| `HOLD_TTL` | `300s` | Hold length in Postgres |
| `REDIS_LOCK_EXTRA` | `10s` | Redis lock lives `HOLD_TTL + extra` = 310s |

## Known limitations (Phase 2)

- **Hold hoarding.** A user can re-hold the same seat as soon as their hold expires, and keep doing it, so one user could keep a seat indefinitely. Not fixed yet; a later phase will add a per-user re-hold cooldown or limit.
- **10-second Redis/DB window.** The Redis lock lives 10s longer than the database hold (310s vs 300s, on purpose). For up to 10s after a hold expires, the seat map shows `AVAILABLE` (from Postgres), but a new hold by *another* user can still get `409 SEAT_HELD` from Redis. This errs on the safe side.
- **Best-effort Redis release.** After a cancel or confirm, the Redis lock is deleted on a best-effort basis. If Redis is down, it lingers until its TTL. Phase 4's transactional outbox will make this reliable.

## Project layout

```
cmd/api/            HTTP server entry point (wiring, graceful shutdown)
cmd/migrate/        `make migrate-up` / `make migrate-down`
internal/config/    environment variables
internal/db/        pgx pool + migration runner
internal/booking/   domain: models, errors, service (rules), repository (SQL), SeatLocker interface
internal/lock/      Redis implementation of SeatLocker (SET NX EX, Lua release)
internal/httpapi/   handlers, router, JSON helpers, error -> HTTP status mapping
db/migrations/      SQL schema (embedded into the binary)
db/seed/            demo data
docs/               Swagger output + learning notes
demo/               race.sh, redis-down.sh, verify.sql
```

## Make targets

`up` · `down` · `logs` · `run` · `test` · `test-integration` · `migrate-up` · `migrate-down` · `seed` · `swagger` · `lint`

`make swagger` needs the swag CLI once: `go install github.com/swaggo/swag/cmd/swag@v1.16.4`

## Phase status

- [x] Phase 0: Project setup (Docker Compose, Makefile, config)
- [x] Phase 1: Basic booking API with UNIQUE-constraint safety net + concurrency proof
- [x] Phase 2: Redis seat holds (`SET NX EX 310`), PostgreSQL as authority for holds, fail-closed
- [ ] Phase 3: Mock payment service with idempotency keys, background worker
- [ ] Phase 4: Transactional outbox, refunds
- [ ] Phase 5: Prometheus + Grafana
- [ ] Phase 6: k6 load tests
- [ ] Phase 7: Chaos scripts, CI
- [ ] Phase 8: To be defined
