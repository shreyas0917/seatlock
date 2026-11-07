// Package config reads the application's settings from environment variables.
//
// Where it fits: cmd/api/main.go calls config.Load() once at startup and passes
// the values to the database pool, the migration runner and the HTTP server.
// Nothing else in the code reads environment variables directly, so all
// settings are visible in one place.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every setting the API needs.
type Config struct {
	// HTTPAddr is the address the HTTP server listens on, e.g. ":8080".
	HTTPAddr string
	// DatabaseURL is the PostgreSQL connection string.
	DatabaseURL string
	// RunMigrations, when true, applies pending database migrations on startup.
	RunMigrations bool
	// LogLevel controls how much is logged: debug, info, warn or error.
	LogLevel slog.Level

	// DBMaxConns is the most connections the pool will open to Postgres.
	// pgx's default is max(4, number of CPUs), far too small for a burst of
	// booking requests: extra requests queue for a connection, and that wait
	// eats into their query timeout.
	DBMaxConns int32
	// DBMinConns connections are kept open even when idle, so the first
	// requests after a quiet period do not all pay the connection handshake.
	DBMinConns int32
	// DBQueryTimeout caps one database call (or one transaction), INCLUDING
	// the time spent waiting for a free pool connection.
	DBQueryTimeout time.Duration

	// RedisAddr is host:port of Redis, used for the fast seat lock.
	RedisAddr string
	// Redis timeouts are deliberately SHORT (200ms). Redis answers in well
	// under a millisecond when healthy; if it does not answer quickly it is
	// down or overloaded, and we want to fail closed fast (503) rather than
	// make every hold request hang.
	RedisDialTimeout  time.Duration
	RedisReadTimeout  time.Duration
	RedisWriteTimeout time.Duration
	// RedisPoolSize caps open Redis connections; RedisMinIdleConns keeps that
	// many open even when idle. Together they keep the pool WARM: a burst of
	// hold requests queues briefly for an already-open connection (Redis
	// answers in ~1ms) instead of opening dozens of new TCP connections at
	// once, whose first replies can exceed the 200ms timeouts and fail closed.
	RedisPoolSize     int32
	RedisMinIdleConns int32

	// HoldTTL is how long a seat hold lasts in PostgreSQL (default 5 minutes).
	HoldTTL time.Duration
	// RedisLockExtra is added to HoldTTL for the Redis lock's lifetime, so the
	// Redis lock always outlives the database hold (see internal/lock).
	RedisLockExtra time.Duration

	// PaymentURL is the base URL of the payment gateway (the mock in this project).
	PaymentURL string
	// IdempotencyLease is how long a request owns an Idempotency-Key before
	// a retry may take it over. Must be longer than one whole confirm
	// (two transactions + PaymentTimeout).
	IdempotencyLease time.Duration

	// MetricsAddr is where /metrics is served: a SEPARATE, internal port, not
	// the public API port. Empty = the binary's default (api :9091, worker
	// :9092).
	MetricsAddr string

	// PendingHoldMax: how long past its expiry a hold with an unresolved
	// (PENDING) payment may keep blocking its seat. Used by the API and the
	// worker alike (booking.Limits).
	PendingHoldMax time.Duration
	// OutboxMaxAttempts: attempts before an outbox row becomes a FAILED dead letter.
	OutboxMaxAttempts int32

	// Worker (cmd/worker) settings. Each job claims at most WorkerBatchSize
	// rows per run.
	WorkerBatchSize int32
	// Payment reconciler: checks PENDING payments older than ReconcileAfter.
	ReconcileInterval time.Duration
	ReconcileAfter    time.Duration
	ReconcileLease    time.Duration
	// Outbox drainer.
	OutboxInterval time.Duration
	OutboxLease    time.Duration
	// Hold expirer.
	ExpireInterval time.Duration
	// Idempotency key cleanup: deletes COMPLETED keys older than IdempotencyRetention.
	CleanupInterval      time.Duration
	IdempotencyRetention time.Duration
	// PaymentTimeout caps one call to the gateway. When it fires, the outcome
	// of a charge is UNKNOWN (the payment stays PENDING, 504 to the client).
	PaymentTimeout time.Duration
}

// Load builds a Config from environment variables, using a sensible default
// for anything that is not set. It returns an error only when a value is set
// but invalid (for example RUN_MIGRATIONS=maybe), because silently ignoring a
// typo in production config is worse than refusing to start.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:    getEnv("HTTP_ADDR", ":8080"),
		DatabaseURL: getEnv("DATABASE_URL", "postgres://seatlock:seatlock@localhost:5434/seatlock?sslmode=disable"),
	}

	runMigrations, err := strconv.ParseBool(getEnv("RUN_MIGRATIONS", "true"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid RUN_MIGRATIONS: %w", err)
	}
	cfg.RunMigrations = runMigrations

	level, err := parseLogLevel(getEnv("LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level

	if cfg.DBMaxConns, err = getInt32("DB_MAX_CONNS", 25); err != nil {
		return Config{}, err
	}
	if cfg.DBMinConns, err = getInt32("DB_MIN_CONNS", 5); err != nil {
		return Config{}, err
	}
	if cfg.DBMaxConns < 1 || cfg.DBMinConns < 0 || cfg.DBMinConns > cfg.DBMaxConns {
		return Config{}, fmt.Errorf("invalid pool size: need 0 <= DB_MIN_CONNS (%d) <= DB_MAX_CONNS (%d) and DB_MAX_CONNS >= 1",
			cfg.DBMinConns, cfg.DBMaxConns)
	}

	cfg.RedisAddr = getEnv("REDIS_ADDR", "localhost:6379")
	cfg.PaymentURL = getEnv("PAYMENT_URL", "http://localhost:8095")
	cfg.MetricsAddr = getEnv("METRICS_ADDR", "")
	if cfg.RedisPoolSize, err = getInt32("REDIS_POOL_SIZE", 10); err != nil {
		return Config{}, err
	}
	if cfg.RedisMinIdleConns, err = getInt32("REDIS_MIN_IDLE_CONNS", 10); err != nil {
		return Config{}, err
	}
	if cfg.RedisPoolSize < 1 || cfg.RedisMinIdleConns < 0 || cfg.RedisMinIdleConns > cfg.RedisPoolSize {
		return Config{}, fmt.Errorf("invalid redis pool: need 0 <= REDIS_MIN_IDLE_CONNS (%d) <= REDIS_POOL_SIZE (%d) and REDIS_POOL_SIZE >= 1",
			cfg.RedisMinIdleConns, cfg.RedisPoolSize)
	}
	durations := []struct {
		key      string
		fallback time.Duration
		dst      *time.Duration
	}{
		{"DB_QUERY_TIMEOUT", 3 * time.Second, &cfg.DBQueryTimeout},
		{"REDIS_DIAL_TIMEOUT", 200 * time.Millisecond, &cfg.RedisDialTimeout},
		{"REDIS_READ_TIMEOUT", 200 * time.Millisecond, &cfg.RedisReadTimeout},
		{"REDIS_WRITE_TIMEOUT", 200 * time.Millisecond, &cfg.RedisWriteTimeout},
		{"HOLD_TTL", 300 * time.Second, &cfg.HoldTTL},
		{"REDIS_LOCK_EXTRA", 10 * time.Second, &cfg.RedisLockExtra},
		{"PAYMENT_TIMEOUT", 5 * time.Second, &cfg.PaymentTimeout},
		{"IDEMPOTENCY_LEASE", 30 * time.Second, &cfg.IdempotencyLease},
		{"PENDING_HOLD_MAX", 10 * time.Minute, &cfg.PendingHoldMax},
		{"RECONCILE_INTERVAL", 30 * time.Second, &cfg.ReconcileInterval},
		{"RECONCILE_AFTER", 2 * time.Minute, &cfg.ReconcileAfter},
		{"RECONCILE_LEASE", 60 * time.Second, &cfg.ReconcileLease},
		{"OUTBOX_INTERVAL", 5 * time.Second, &cfg.OutboxInterval},
		{"OUTBOX_LEASE", 60 * time.Second, &cfg.OutboxLease},
		{"EXPIRE_INTERVAL", 10 * time.Second, &cfg.ExpireInterval},
		{"CLEANUP_INTERVAL", time.Hour, &cfg.CleanupInterval},
		{"IDEMPOTENCY_RETENTION", 24 * time.Hour, &cfg.IdempotencyRetention},
	}
	for _, d := range durations {
		value, err := getDuration(d.key, d.fallback)
		if err != nil {
			return Config{}, err
		}
		if value <= 0 {
			return Config{}, fmt.Errorf("invalid %s: must be greater than zero", d.key)
		}
		*d.dst = value
	}

	if cfg.OutboxMaxAttempts, err = getInt32("OUTBOX_MAX_ATTEMPTS", 20); err != nil {
		return Config{}, err
	}
	if cfg.WorkerBatchSize, err = getInt32("WORKER_BATCH_SIZE", 50); err != nil {
		return Config{}, err
	}
	if cfg.OutboxMaxAttempts < 1 || cfg.WorkerBatchSize < 1 {
		return Config{}, fmt.Errorf("OUTBOX_MAX_ATTEMPTS and WORKER_BATCH_SIZE must be at least 1")
	}

	return cfg, nil
}

// ValidateWorker checks that the worker's timings are SAFE together, and is
// called by cmd/worker before it starts. Wrong values here do not just make
// things slow, they make them WRONG, so the worker refuses to start:
//
//   - RECONCILE_AFTER > PAYMENT_TIMEOUT + IDEMPOTENCY_LEASE: the reconciler
//     must never touch a payment whose own request may still be running
//     (that request can last up to the payment timeout, and its client may
//     retry within the idempotency lease).
//   - PENDING_HOLD_MAX > RECONCILE_AFTER: the reconciler must get a chance to
//     resolve a payment before its seat is handed to someone else.
//   - RECONCILE_LEASE > 2 x PAYMENT_TIMEOUT + DB_QUERY_TIMEOUT: one check does
//     up to two gateway calls (lookup, void) plus Tx B. If the lease ran out
//     earlier, a second worker could claim the same payment mid-check.
//   - OUTBOX_LEASE > PAYMENT_TIMEOUT + DB_QUERY_TIMEOUT: same, for one refund.
//   - IDEMPOTENCY_RETENTION > IDEMPOTENCY_LEASE: never delete a key while its
//     request could still be running.
func (c Config) ValidateWorker() error {
	checks := []struct {
		ok   bool
		rule string
	}{
		{c.ReconcileAfter > c.PaymentTimeout+c.IdempotencyLease,
			fmt.Sprintf("RECONCILE_AFTER (%s) must be greater than PAYMENT_TIMEOUT + IDEMPOTENCY_LEASE (%s)", c.ReconcileAfter, c.PaymentTimeout+c.IdempotencyLease)},
		{c.PendingHoldMax > c.ReconcileAfter,
			fmt.Sprintf("PENDING_HOLD_MAX (%s) must be greater than RECONCILE_AFTER (%s)", c.PendingHoldMax, c.ReconcileAfter)},
		{c.ReconcileLease > 2*c.PaymentTimeout+c.DBQueryTimeout,
			fmt.Sprintf("RECONCILE_LEASE (%s) must be greater than 2 x PAYMENT_TIMEOUT + DB_QUERY_TIMEOUT (%s)", c.ReconcileLease, 2*c.PaymentTimeout+c.DBQueryTimeout)},
		{c.OutboxLease > c.PaymentTimeout+c.DBQueryTimeout,
			fmt.Sprintf("OUTBOX_LEASE (%s) must be greater than PAYMENT_TIMEOUT + DB_QUERY_TIMEOUT (%s)", c.OutboxLease, c.PaymentTimeout+c.DBQueryTimeout)},
		{c.IdempotencyRetention > c.IdempotencyLease,
			fmt.Sprintf("IDEMPOTENCY_RETENTION (%s) must be greater than IDEMPOTENCY_LEASE (%s)", c.IdempotencyRetention, c.IdempotencyLease)},
	}
	for _, check := range checks {
		if !check.ok {
			return fmt.Errorf("unsafe worker configuration: %s", check.rule)
		}
	}
	return nil
}

// getDuration reads a Go duration such as "300s", "200ms" or "5m".
func getDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := getEnv(key, "")
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q (use a duration like 300s or 200ms): %w", key, raw, err)
	}
	return d, nil
}

// getEnv returns the value of an environment variable, or fallback if it is
// unset or empty.
func getEnv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// getInt32 reads a whole number from an environment variable.
func getInt32(key string, fallback int32) (int32, error) {
	raw := getEnv(key, "")
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", key, raw, err)
	}
	return int32(n), nil
}

// parseLogLevel turns "debug" / "info" / "warn" / "error" into a slog.Level.
func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(value) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid LOG_LEVEL %q (use debug, info, warn or error)", value)
	}
}
