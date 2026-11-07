# Phase 1 study notes: basic booking API

> **Note (Phase 2):** direct booking (`POST /v1/bookings` with `show_id` + `seat_id`) was replaced by hold → confirm. `POST /v1/bookings` now takes a `hold_id`. Everything below about the UNIQUE constraint and 23505 still applies; it is still the final safety net. See [phase-2.md](phase-2.md).

Goal of Phase 1: a booking API where **one seat can never be booked twice**, even when many requests arrive at the same moment, and a test that **proves** it.

---

## 1. The request flow

```
  client (curl / browser / k6)
     │  POST /v1/bookings   X-User-ID: alice   {"show_id":1,"seat_id":7}
     ▼
┌──────────────────────────────────────────────────────────────┐
│ http.Server              cmd/api/main.go                     │
│   timeouts, graceful shutdown                                │
├──────────────────────────────────────────────────────────────┤
│ chi router + middleware  internal/httpapi/router.go          │
│   RequestID → requestLogger → Recoverer                      │
├──────────────────────────────────────────────────────────────┤
│ handler                  internal/httpapi/handlers.go        │
│   read JSON + header, call service, write JSON                │
│   errors → mapError()    internal/httpapi/errors.go          │
├──────────────────────────────────────────────────────────────┤
│ booking.Service          internal/booking/service.go         │
│   business rules: user present? ids positive? seat in show?  │
├──────────────────────────────────────────────────────────────┤
│ PostgresRepository       internal/booking/repository_postgres.go │
│   hand-written SQL, 23505 → ErrSeatAlreadyBooked              │
├──────────────────────────────────────────────────────────────┤
│ PostgreSQL               db/migrations/0001_init.up.sql      │
│   UNIQUE (show_id, seat_id)  ← the real safety net            │
└──────────────────────────────────────────────────────────────┘
```

The response travels back up: `Booking` → `BookingResponse` JSON with **201**, or a domain error → `mapError` → e.g. **409** `SEAT_ALREADY_BOOKED`.

**Layer rules:** handlers only do HTTP, the service only does business rules, and the repository only does SQL. Each layer only talks to the one directly below it.

---

## 2. What each folder and file does

| Path | What it does |
|---|---|
| `cmd/api/main.go` | Entry point. Wires config → logger → migrations → pool → repository → service → router → server. Handles graceful shutdown. |
| `cmd/migrate/main.go` | Small CLI used by `make migrate-up` / `make migrate-down`. |
| `internal/config/config.go` | Reads `HTTP_ADDR`, `DATABASE_URL`, `RUN_MIGRATIONS`, `LOG_LEVEL`, with defaults. |
| `internal/db/pool.go` | Creates the pgx connection pool and pings the database once. |
| `internal/db/migrate.go` | Runs the embedded SQL migrations with golang-migrate. |
| `internal/booking/models.go` | Plain domain types: `Show`, `Seat`, `SeatWithStatus`, `Booking`, `BookSeatInput`. |
| `internal/booking/errors.go` | Sentinel errors such as `ErrSeatAlreadyBooked` and `ErrShowNotFound`. |
| `internal/booking/service.go` | `Repository` interface + `Service` with the business rules. |
| `internal/booking/repository_postgres.go` | All SQL. Maps `pgx.ErrNoRows` → not-found, and `23505` → `ErrSeatAlreadyBooked`. |
| `internal/booking/service_test.go` | Unit tests with an in-memory fake repository. |
| `internal/booking/repository_postgres_test.go` | Integration tests + **the 50-goroutine concurrency proof**. |
| `internal/httpapi/router.go` | URLs → handlers, middleware, per-request JSON log line. |
| `internal/httpapi/handlers.go` | One function per endpoint, plus swag annotations. |
| `internal/httpapi/errors.go` | **The single place** that maps domain errors → HTTP status + error code. |
| `internal/httpapi/json.go` | `writeJSON` and `decodeJSON` (size limit, unknown fields rejected). |
| `internal/httpapi/responses.go` | JSON request/response structs (DTOs) with struct tags. |
| `internal/httpapi/handlers_test.go` | `httptest` tests for 201 / 400 / 404 / 409 / 500 / readyz. |
| `db/migrations/` | `0001_init.up.sql` / `.down.sql` + `migrations.go` (embeds them in the binary). |
| `db/seed/seed.sql` | 1 show, 100 seats, idempotent. |
| `db/init/01-create-test-db.sql` | Creates the `seatlock_test` database the first time the Postgres container starts. |
| `demo/race.sh`, `demo/verify.sql` | Fire 20 parallel HTTP bookings at one seat; check no seat has 2 bookings. |
| `docs/` | Generated Swagger files (`docs.go`, `swagger.json`, `swagger.yaml`) + these notes. |

---

## 3. Why the UNIQUE constraint prevents double booking

### The wrong way: check-then-insert

```go
// DON'T DO THIS
booked := repo.IsSeatBooked(showID, seatID)   // step 1: check
if !booked {
    repo.InsertBooking(showID, seatID, user)    // step 2: insert
}
```

Two users, Alice and Bob, click "Book seat A7" at the same time:

```
time   Alice's request                   Bob's request
────   ───────────────────────────────   ───────────────────────────────
t1     SELECT … seat A7 → free ✅
t2                                       SELECT … seat A7 → free ✅
t3     INSERT booking (A7, alice) ✅
t4                                       INSERT booking (A7, bob) ✅   ← DOUBLE BOOKING 💥
```

The check and the insert are two separate steps. Between them there is a gap, and the other request slips in. Both saw "free", so both inserted.

A Go `sync.Mutex` would not fix this either: in production there are several API servers, and each has its own memory and its own mutex.

### The right way: let the database decide

```sql
CONSTRAINT bookings_show_seat_unique UNIQUE (show_id, seat_id)
```

```go
INSERT INTO bookings (show_id, seat_id, user_id) VALUES ($1, $2, $3)
```

```
time   Alice's request                   Bob's request
────   ───────────────────────────────   ───────────────────────────────
t1     INSERT (A7, alice)                INSERT (A7, bob)
       ── Postgres checks the unique index atomically ──
t2     ✅ committed → 201 Created         ❌ ERROR 23505 → 409 Conflict
```

Every API server, goroutine and request goes through the **same** database. The unique index check and the insert happen as **one atomic step** inside Postgres. If two transactions try to insert the same key, the second one waits for the first; when the first commits, the second fails. Exactly one can ever win.

We still validate in the service (show exists, seat exists, seat belongs to show), but that is **for nice error messages** (400/404), not for safety. Safety comes only from the constraint.

Extra safety in the schema: `bookings_seat_fk FOREIGN KEY (show_id, seat_id) REFERENCES seats (show_id, id)` means the database itself refuses a booking whose seat belongs to a different show, even if the Go code had a bug.

---

## 4. What error 23505 is and how it becomes 409

PostgreSQL returns a 5-character **SQLSTATE** code with every error. `23505` = `unique_violation` (class `23` = integrity constraint violation). `23503` = `foreign_key_violation`.

The path through the code:

1. **Postgres** rejects the INSERT: `ERROR: duplicate key value violates unique constraint "bookings_show_seat_unique" (SQLSTATE 23505)`.
2. **pgx** returns it as a `*pgconn.PgError` with `.Code = "23505"` and `.ConstraintName = "bookings_show_seat_unique"`.
3. **Repository** (`mapInsertBookingError` in `repository_postgres.go`):
   ```go
   var pgErr *pgconn.PgError
   if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
      pgErr.ConstraintName == "bookings_show_seat_unique" {
       return ErrSeatAlreadyBooked
   }
   ```
   We check the **constraint name** too, so a unique violation on some *other* constraint added later is not mistaken for "seat taken".
4. **Service** returns the error unchanged.
5. **`mapError`** in `internal/httpapi/errors.go`: `errors.Is(err, booking.ErrSeatAlreadyBooked)` → `409`, code `SEAT_ALREADY_BOOKED`.

Any error that is *not* a known domain error becomes **500 `INTERNAL_ERROR`**. The real error is logged with the request ID, but never sent to the client (it might contain SQL, table names or hostnames).

---

## 5. Go concepts used (and where to find them)

| Concept | Short explanation | Where |
|---|---|---|
| **package / internal/** | Code under `internal/` can only be imported by this module, so it is private to SeatLock. | folder layout |
| **`context.Context`** | Carries a deadline + cancel signal through calls. When it expires, DB calls stop waiting. Always the first parameter. | `internal/db/pool.go`, every repository method |
| **`context.WithTimeout`** | Makes a child context that expires after a duration. We use 3s per query and 1s for `/readyz`. | `repository_postgres.go`, `handlers.go` (readyz) |
| **`defer`** | Runs a call when the function returns, on every path. Used for `cancel()`, `rows.Close()` and `pool.Close()`. | `pool.go`, `repository_postgres.go`, `main.go` |
| **Interfaces** | A set of method signatures. Any type with those methods satisfies it, with no `implements` keyword. Lets us swap Postgres for a fake in tests. | `Repository` in `service.go`; `BookingService`, `Pinger` in `handlers.go` |
| **Compile-time interface check** | `var _ Repository = (*PostgresRepository)(nil)` breaks the build if a method is missing. | `repository_postgres.go` |
| **Error wrapping `%w`** | `fmt.Errorf("get show %d: %w", id, err)` adds context but keeps the original error inside. | everywhere in repository/service |
| **`errors.Is`** | "Is this error (or anything it wraps) equal to X?" Used with sentinel errors. | `httpapi/errors.go`, `db/migrate.go`, tests |
| **`errors.As`** | "Is there an error of *type* T in the chain? If yes, give it to me." Used to read the Postgres error code. | `mapInsertBookingError` in `repository_postgres.go` |
| **Sentinel errors** | Package-level `errors.New(...)` values compared with `errors.Is`. | `internal/booking/errors.go` |
| **Struct tags** | `` `json:"show_id"` `` tells `encoding/json` the key name; `example:` is for Swagger. | `internal/httpapi/responses.go` |
| **Struct embedding** | `SeatWithStatus` embeds `Seat`, so `s.Label` works directly. | `internal/booking/models.go` |
| **Named types + consts** | `type SeatStatus string` with `SeatAvailable`, `SeatBooked`. | `models.go` |
| **pgx pool** | A shared set of open DB connections, safe for concurrent use. One per app. | `internal/db/pool.go` |
| **Goroutines** | `go f()` runs `f` concurrently. Cheap (a few KB each). | `main.go` (server), concurrency test |
| **Channels** | Typed pipes between goroutines. Closing a channel wakes every receiver, which we use as a "starting gun". | concurrency test; `serverErr` in `main.go` |
| **`select`** | Waits on several channels and runs whichever is ready first. | `main.go` (server error vs shutdown signal) |
| **`sync.WaitGroup`** | Counter: `Add(1)` per goroutine, `Done()` when it ends, `Wait()` until 0. | concurrency test |
| **`sync.Mutex`** | Lock so only one goroutine touches shared data at a time. | fake repository in `service_test.go` |
| **`signal.NotifyContext`** | A context that is cancelled on Ctrl+C / SIGTERM, used to trigger graceful shutdown. | `main.go` |
| **`//go:embed`** | Copies files into the compiled binary. | `db/migrations/migrations.go` |
| **Blank import `_`** | Imports a package only for its side effects (its `init()` registers something). | migrate pgx driver in `db/migrate.go`; swagger docs in `main.go` |
| **Build tags** | `//go:build integration` compiles a file only with `-tags=integration`. | `repository_postgres_test.go` |
| **Table-driven tests + `t.Run`** | A slice of cases run in a loop as named subtests. | `service_test.go`, `handlers_test.go` |
| **`httptest`** | Fake requests/recorders to test handlers without a real server. | `handlers_test.go` |
| **`log/slog`** | Structured JSON logging: `logger.Info("msg", "key", value)`. | `router.go`, `errors.go`, `main.go` |

---

## 6. Likely interview questions (with short answers)

**Q1. How do you prevent two users from booking the same seat?**
A `UNIQUE (show_id, seat_id)` constraint on the `bookings` table. The app just INSERTs. Postgres guarantees only one insert per (show, seat) can commit, and the loser gets error 23505, which we return as 409 Conflict.

**Q2. Why not check whether the seat is free and then insert?**
It's a race condition (TOCTOU, time-of-check to time-of-use). Two requests can both see "free" before either inserts, and then both insert. The check and the write must be one atomic operation, and the unique constraint gives us exactly that.

**Q3. Why not use a mutex in Go?**
A mutex only protects one process. With 2+ API instances behind a load balancer, each has its own mutex. The database is the only component shared by all requests.

**Q4. How do you prove it works?**
An integration test starts 50 goroutines, holds them behind a closed-channel "starting gun", releases them together, and asserts exactly 1 success, 49 `ErrSeatAlreadyBooked`, and exactly 1 row in the table. `demo/race.sh` does the same with 20 parallel HTTP requests, and `verify.sql` confirms no duplicates.

**Q5. What is SQLSTATE 23505 and how do you handle it?**
`unique_violation`. In the repository, `errors.As` extracts the `*pgconn.PgError`, and we check `Code == "23505"` **and** the constraint name, then return the domain error `ErrSeatAlreadyBooked`. One function in the HTTP layer maps that to 409.

**Q6. Why map database errors to domain errors instead of returning them directly?**
Layering and security. Handlers shouldn't know about Postgres. If storage changes, only the repository changes. And raw DB errors can leak internals, so unknown errors become a generic 500 and are logged with the request ID.

**Q7. Why store money as integer paise?**
Floats are binary and can't represent most decimals exactly (0.1 + 0.2 ≠ 0.3), so rounding errors creep in. Integers in the smallest unit are exact.

**Q8. What do `/healthz` and `/readyz` mean?**
Liveness vs readiness. `/healthz` = "the process is running" (if it fails, restart me). `/readyz` = "I can serve traffic" (DB ping within 1s, else 503, so the load balancer stops sending traffic but doesn't kill the process).

**Q9. How does graceful shutdown work?**
`signal.NotifyContext` cancels a context on SIGINT/SIGTERM. Then `server.Shutdown(ctx)` stops accepting connections, waits (up to 15s) for in-flight requests to finish, and finally `defer pool.Close()` closes DB connections. No request is cut off halfway.

**Q10. Why use `context.Context` and timeouts everywhere?**
So nothing waits forever. If Postgres hangs, the 3s query timeout returns an error instead of piling up goroutines and connections. If the client disconnects, the request context is cancelled and the query is abandoned.

**Bonus. What does the composite foreign key do?**
`FOREIGN KEY (show_id, seat_id) REFERENCES seats (show_id, id)` makes the database reject a booking whose seat belongs to another show, so it is defence in depth behind the service's validation.

---

## 7. What changes in Phase 2

Right now booking is one step: click → INSERT. Real ticketing is two steps: **hold** a seat for a few minutes while the user pays, then **confirm**. In Phase 2 we add seat holds using Redis `SET key value NX EX 310` (set only if not already set, auto-expire after 310s), so holds are fast and expire on their own. PostgreSQL stays the authority: hold ownership is also recorded there, and the `UNIQUE(show_id, seat_id)` constraint remains the final guard on bookings. Because SeatLock is **fail-closed**, a Redis outage must never let through a booking that PostgreSQL has not authorized. The current layering (handler → service → `Repository` interface) means the hold logic slots into the service and a new store, without rewriting the handlers.
