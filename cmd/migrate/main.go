// Command migrate applies or rolls back database migrations from the terminal.
//
// Usage (normally through the Makefile):
//
//	go run ./cmd/migrate up     # apply all pending migrations  (make migrate-up)
//	go run ./cmd/migrate down   # roll back the latest migration (make migrate-down)
//
// It uses the same DATABASE_URL and the same migration code as the API
// (internal/db/migrate.go), so there is only one way migrations get applied.
package main

import (
	"fmt"
	"os"

	"github.com/shreyas0917/seatlock/internal/config"
	"github.com/shreyas0917/seatlock/internal/db"
)

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "up" && os.Args[1] != "down") {
		fmt.Fprintln(os.Stderr, "usage: migrate up|down")
		os.Exit(2)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	if os.Args[1] == "up" {
		err = db.MigrateUp(cfg.DatabaseURL)
	} else {
		err = db.MigrateDown(cfg.DatabaseURL)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("migrate", os.Args[1], "OK")
}
