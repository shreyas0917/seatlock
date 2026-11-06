# SeatLock

**A high-concurrency ticket booking backend in Go that guarantees a seat can never be sold twice, even when 50 users click "Book" at the same instant.**

SeatLock is a backend-only system design project (think BookMyShow). It follows a **fail-closed booking policy**: PostgreSQL is the source of truth, and no cache failure can ever produce an unauthorized booking.

## Tech stack

Go · chi · PostgreSQL 16 (pgx, hand-written SQL) · golang-migrate · slog · Swagger (swag) · Docker Compose · Redis 7 (wired from Phase 2)

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

```bash
curl localhost:8080/v1/shows
curl localhost:8080/v1/shows/1/seats
curl -X POST localhost:8080/v1/bookings -H "X-User-ID: alice" -d '{"show_id":1,"seat_id":7}'
```

## API

User identity comes from the `X-User-ID` header (there is no auth in this project). It is required on write endpoints.

| Method | Path | Description | Responses |
|---|---|---|---|
| GET | `/healthz` | Process is alive | 200 |
| GET | `/readyz` | Postgres answers a ping within 1s | 200, 503 |
| GET | `/v1/shows` | List shows | 200 |
| GET | `/v1/shows/{showID}/seats` | Seat map: `label`, `price_paise`, `status` (`AVAILABLE`/`BOOKED`) | 200, 400, 404 |
| POST | `/v1/bookings` | Book a seat: `{"show_id":1,"seat_id":7}` | 201, 400, 404, **409** |
| GET | `/v1/bookings/{bookingID}` | Get one booking | 200, 400, 404 |

All errors share one shape:

```json
{ "error": { "code": "SEAT_ALREADY_BOOKED", "message": "this seat has already been booked" } }
```

## How double booking is prevented

The `bookings` table has `CONSTRAINT bookings_show_seat_unique UNIQUE (show_id, seat_id)`.
The API never does "check if free, then insert" (a race condition). It just inserts, and PostgreSQL decides atomically: one insert wins, and every other one fails with error `23505`, which becomes **409 Conflict**.
Full explanation: [docs/learning/phase-1.md](docs/learning/phase-1.md).

## Tests

```bash
make test               # unit + handler tests (no database)
make test-integration   # + real Postgres tests, including the 50-goroutine concurrency proof
make lint               # go vet + gofmt check
```

The concurrency proof (`TestConcurrentBookingSameSeat` in [internal/booking/repository_postgres_test.go](internal/booking/repository_postgres_test.go)) starts 50 goroutines that book the same seat at the same moment and asserts **1 success, 49 `ErrSeatAlreadyBooked`, 1 row in the table**.

Integration tests use a separate `seatlock_test` database, so they never touch your seeded data.

## Race demo over HTTP

```bash
./demo/race.sh
```

```
Firing 20 parallel booking requests for show 1, seat 1 ...

201 Created : 1
409 Conflict: 19

demo/verify.sql (must return 0 rows):
 show_id | seat_id | count
---------+---------+-------
(0 rows)

PASS: exactly one booking won the race.
```

Each run books the next available seat.

## Project layout

```
cmd/api/            HTTP server entry point (wiring, graceful shutdown)
cmd/migrate/        `make migrate-up` / `make migrate-down`
internal/config/    environment variables
internal/db/        pgx pool + migration runner
internal/booking/   domain: models, errors, service (rules), repository (SQL)
internal/httpapi/   handlers, router, JSON helpers, error -> HTTP status mapping
db/migrations/      SQL schema (embedded into the binary)
db/seed/            demo data
docs/               Swagger output + learning notes
demo/               race.sh + verify.sql
```

## Make targets

`up` · `down` · `logs` · `run` · `test` · `test-integration` · `migrate-up` · `migrate-down` · `seed` · `swagger` · `lint`

`make swagger` needs the swag CLI once: `go install github.com/swaggo/swag/cmd/swag@v1.16.4`

## Phase status

- [x] Phase 0: Project setup (Docker Compose, Makefile, config)
- [x] Phase 1: Basic booking API with UNIQUE-constraint safety net + concurrency proof
- [ ] Phase 2: Redis seat holds (`SET NX EX 310`), PostgreSQL as authority for hold ownership
- [ ] Phase 3: Mock payment service with idempotency keys, background worker
- [ ] Phase 4: Transactional outbox, refunds
- [ ] Phase 5: Prometheus + Grafana
- [ ] Phase 6: k6 load tests
- [ ] Phase 7: Chaos scripts, CI
- [ ] Phase 8: To be defined
