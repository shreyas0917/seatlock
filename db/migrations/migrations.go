// Package migrations embeds the .sql migration files into the Go binary.
//
// Where it fits: internal/db/migrate.go reads these files to create or update
// the database schema. Because they are embedded at compile time, the API
// binary (and the Docker image) can run migrations without the .sql files
// being present on disk.
package migrations

import "embed"

// FS contains every *.sql file in this folder.
//
// The "//go:embed" line below is a compiler directive (not a normal comment):
// it tells the Go compiler to copy the matching files into the binary.
//
//go:embed *.sql
var FS embed.FS
