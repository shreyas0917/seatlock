// migrate.go applies or rolls back the SQL files in db/migrations.
//
// Where it fits: cmd/api/main.go calls MigrateUp on startup when
// RUN_MIGRATIONS=true, and cmd/migrate (used by `make migrate-up` /
// `make migrate-down`) calls MigrateUp or MigrateDown directly.
//
// golang-migrate records the current version in a table called
// schema_migrations, so each migration runs exactly once per database. It also
// takes a Postgres advisory lock, so two API instances starting at the same
// time will not both try to run the same migration.
package db

import (
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	// The blank import "_" runs the package's init() function, which registers
	// the "pgx5" database driver with golang-migrate. We never call it by name.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/shreyas0917/seatlock/db/migrations"
)

// MigrateUp applies every migration that has not been applied yet.
func MigrateUp(databaseURL string) error {
	m, err := newMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer m.Close()

	// ErrNoChange just means "already up to date", which is not a failure.
	// errors.Is checks whether err IS (or wraps) that specific error value.
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// MigrateDown rolls back exactly ONE migration (the most recent one).
// Rolling back one step at a time is safer than dropping everything.
func MigrateDown(databaseURL string) error {
	m, err := newMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer m.Close()

	if err := m.Steps(-1); err != nil {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// newMigrator connects golang-migrate to the embedded .sql files and the database.
func newMigrator(databaseURL string) (*migrate.Migrate, error) {
	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("load embedded migrations: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, toMigrateURL(databaseURL))
	if err != nil {
		return nil, fmt.Errorf("init migrator: %w", err)
	}
	return m, nil
}

// toMigrateURL swaps the URL scheme so golang-migrate uses its pgx v5 driver.
// "postgres://user:pass@host/db" becomes "pgx5://user:pass@host/db".
func toMigrateURL(databaseURL string) string {
	for _, prefix := range []string{"postgresql://", "postgres://"} {
		if strings.HasPrefix(databaseURL, prefix) {
			return "pgx5://" + strings.TrimPrefix(databaseURL, prefix)
		}
	}
	return databaseURL
}
