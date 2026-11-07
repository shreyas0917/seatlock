# SeatLock developer commands. Run `make <target>`.
# Settings come from .env when present (copy .env.example to .env).

-include .env
export

POSTGRES_USER ?= seatlock
POSTGRES_DB   ?= seatlock
SWAG_VERSION  := v1.16.4
# Extra flags for go test, e.g. `make test TEST_FLAGS=-race` (CI does this on Linux).
TEST_FLAGS    ?=

.PHONY: up up-demo up-bench up-scale seed-load down logs run test test-integration verify verify-sql migrate-up migrate-down seed reset-data outbox-retry swagger lint

## up: build the API image and start postgres, redis and api in the background
up:
	docker compose up -d --build --wait

## up-demo: like `up`, but with the SHORT timings of docker-compose.demo.yml
##          (20s holds, worker every few seconds). Needed by the worker demos.
##          `make up` switches back to the normal timings.
up-demo:
	docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --build --wait

## up-bench: like `up`, with docker-compose.bench.yml (mock gateway answers in
##           50-150 ms, everything else normal). Used for the load tests.
up-bench:
	docker compose -f docker-compose.yml -f docker-compose.bench.yml up -d --build --wait

## up-scale: like up-bench, plus the "scale" profile: a second api replica
##           (api-2) behind an nginx load balancer at http://localhost:8088
##           (http://lb inside the network). Used by chaos/api-replica.sh.
up-scale:
	docker compose -f docker-compose.yml -f docker-compose.bench.yml --profile scale up -d --build --wait

## down: stop all containers (data volume is kept; use `docker compose down -v` to wipe it)
down:
	docker compose --profile scale down

## logs: follow logs from all containers
logs:
	docker compose logs -f

## run: run the API on your machine (Postgres must be up: `make up`)
run:
	go run ./cmd/api

## test: fast unit tests (no database needed)
test:
	go test -count=1 $(TEST_FLAGS) ./...

## test-integration: tests against the real Postgres from docker compose
## -p 1 runs one package at a time: integration tests in different packages
## share the seatlock_test database, so they must not TRUNCATE it in parallel.
test-integration:
	go test -count=1 -p 1 -tags=integration $(TEST_FLAGS) ./...

## verify: gofmt + vet + unit + integration tests, full log in tmp/. Push only if this passes.
verify:
	./scripts/verify.sh

## migrate-up: apply all pending migrations to DATABASE_URL
migrate-up:
	go run ./cmd/migrate up

## migrate-down: roll back ONE migration
migrate-down:
	go run ./cmd/migrate down

## seed: load demo data (safe to run more than once)
seed:
	docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U $(POSTGRES_USER) -d $(POSTGRES_DB) < db/seed/seed.sql

## seed-load: load-test data: a 100-seat rush show, a 20-seat smoke show and
##            200 shows x 200 seats (safe to run more than once)
seed-load:
	docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U $(POSTGRES_USER) -d $(POSTGRES_DB) < db/seed/load.sql

## reset-data: delete all bookings, holds, payments, outbox rows and idempotency keys
##             (keeps shows and seats) AND the Redis seat locks, so the demos have
##             free seats again and Redis agrees with Postgres
reset-data:
	docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U $(POSTGRES_USER) -d $(POSTGRES_DB) -c "TRUNCATE bookings, payments, holds, outbox, idempotency_keys RESTART IDENTITY"
	docker compose exec -T redis sh -c "redis-cli --scan --pattern 'seat:*' | xargs -r redis-cli del"

## outbox-retry: put ONE dead-lettered (FAILED) outbox row back in the queue,
##               after fixing its cause:  make outbox-retry ID=42
##               (the same SQL as booking.PostgresRepository.RequeueOutbox)
outbox-retry:
	@test -n "$(ID)" || (echo "usage: make outbox-retry ID=<outbox id>"; exit 1)
	docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U $(POSTGRES_USER) -d $(POSTGRES_DB) -c "UPDATE outbox SET status = 'PENDING', attempts = 0, next_attempt_at = NOW() WHERE id = $(ID) AND status = 'FAILED' RETURNING id, kind, status"

## verify-sql: regenerate demo/verify.sql from internal/invariants/checks.go
##             (the single source of the invariant checks; a test fails if they differ)
verify-sql:
	go test -count=1 -run TestVerifySQLMatchesTheGoChecks ./internal/invariants/ -update

## swagger: regenerate the OpenAPI docs in ./docs from the handler annotations
##          (needs the swag CLI once: go install github.com/swaggo/swag/cmd/swag@v1.16.4)
swagger:
	@command -v swag >/dev/null || (echo "swag not found. Install it: go install github.com/swaggo/swag/cmd/swag@$(SWAG_VERSION)"; exit 1)
	swag init -g cmd/api/main.go -o docs --parseInternal
	gofmt -w docs/docs.go

## lint: go vet + check formatting (fails if any file is not gofmt-ed)
lint:
	go vet ./...
	go vet -tags=integration ./...
	@test -z "$$(gofmt -l .)" || (echo "Files need gofmt:"; gofmt -l .; exit 1)
	@echo "lint OK"
