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
