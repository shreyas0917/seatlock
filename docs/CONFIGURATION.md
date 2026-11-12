# Configuration

Every setting is an environment variable. `docker-compose.yml` passes them to the containers with the defaults below; for `make run` (the api from source) copy [.env.example](../.env.example) to `.env`. The api and the worker read the **same** settings, so both apply the same rules.

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
| `REDIS_LOCK_EXTRA` | `10s` | Redis lock lives `HOLD_TTL + extra` = 310s ([ADR-0003](adr/0003-redis-lock-outlives-db-hold.md)) |
| `PAYMENT_URL` | `http://localhost:8095` | Payment gateway |
| `PAYMENT_TIMEOUT` | `5s` | One gateway call; on timeout the outcome is unknown (504) |
| `IDEMPOTENCY_LEASE` | `30s` | How long a request owns an `Idempotency-Key` before a retry may take it over |
| `PENDING_HOLD_MAX` | `10m` | How long past expiry a hold with a `PENDING` payment keeps its seat (api + worker) |
| `OUTBOX_MAX_ATTEMPTS` | `20` | Attempts before an outbox row becomes a `FAILED` dead letter |
| `WORKER_BATCH_SIZE` | `50` | Rows per job run |
| `RECONCILE_INTERVAL` / `RECONCILE_AFTER` / `RECONCILE_LEASE` | `30s` / `2m` / `60s` | Payment reconciler |
| `OUTBOX_INTERVAL` / `OUTBOX_LEASE` | `5s` / `60s` | Outbox drainer |
| `EXPIRE_INTERVAL` | `10s` | Hold expirer |
| `CLEANUP_INTERVAL` / `IDEMPOTENCY_RETENTION` | `1h` / `24h` | Idempotency key cleanup |
| `INVARIANTS_INTERVAL` / `INVARIANT_STATEMENT_TIMEOUT` | `15s` / `2s` | Invariant checks as live gauges, and the max time per check query |
| `METRICS_ADDR` | api `:9091`, worker `:9092` | Internal port for `/metrics` and `/debug/pprof` (never the public API port) |
| `PPROF_BLOCK_RATE` / `PPROF_MUTEX_FRACTION` | `0` / `0` (off) | Go block and mutex profiles; turn on only while profiling |
| `PROMETHEUS_PORT` / `GRAFANA_PORT` / `GRAFANA_ADMIN_PASSWORD` | `9095` / `3001` / `admin` | Observability (Grafana viewing needs no login) |
| `LB_PORT` | `8088` | nginx load balancer of the `scale` profile (`make up-scale`) |
| `PAYMENT_PORT` / `PAYMENT_FAIL_RATE` / `PAYMENT_MIN_DELAY` / `PAYMENT_MAX_DELAY` / `PAYMENT_SLOW_DELAY` | `8095` / `0` / `0s` / `0s` / `3s` | Mock gateway (demos; `make up-bench` sets a 50–150 ms delay) |

Host ports used by `make up`: api 8080, Postgres 5434, Redis 6379, mock gateway 8095, Prometheus 9095, Grafana 3001.

**Safety checks:** the worker refuses to start with timings that could break a guarantee, for example `RECONCILE_AFTER` must be greater than `PAYMENT_TIMEOUT + IDEMPOTENCY_LEASE`, so the reconciler never touches a payment a live request may still finish ([internal/config](../internal/config/config.go)).

**Compose overlays:** `make up-demo` ([docker-compose.demo.yml](../docker-compose.demo.yml): 20s holds, fast worker intervals, for the worker demos), `make up-bench` ([docker-compose.bench.yml](../docker-compose.bench.yml): 50–150 ms gateway, for the load tests), `make up-scale` (adds a second api replica behind nginx). `make up` switches back to the normal settings.
