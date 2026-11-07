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

	return cfg, nil
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
