# SeatLock developer commands. Run `make <target>`.
# Settings come from .env when present (copy .env.example to .env).

-include .env
export

POSTGRES_USER ?= seatlock
POSTGRES_DB   ?= seatlock
SWAG_VERSION  := v1.16.4
# Extra flags for go test, e.g. `make test TEST_FLAGS=-race` (CI does this on Linux).
TEST_FLAGS    ?=

.PHONY: up down logs run test test-integration verify migrate-up migrate-down seed swagger lint

## up: build the API image and start postgres, redis and api in the background
up:
	docker compose up -d --build --wait

## down: stop all containers (data volume is kept; use `docker compose down -v` to wipe it)
down:
	docker compose down

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
