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

	return cfg, nil
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
