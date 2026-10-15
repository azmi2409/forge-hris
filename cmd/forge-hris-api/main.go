// Command forge-hris-api runs the forge-hris component 1 REST API
// (identity, tenancy, audit).
//
// Storage is selected by DATABASE_URL:
//   - set: Postgres via the pgx adapter (schema auto-applied on boot).
//   - unset: in-memory store (data lost on restart; fine for dev).
//
// Configuration via environment:
//
//	PORT          listen address port (default 8080)
//	DATABASE_URL  postgres connection string (optional)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/azmi2409/forge-hris/api"
	"github.com/azmi2409/forge-hris/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	var st store.Store
	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		logger.Info("applying schema")
		if err := store.ApplyMigration(ctx, dbURL); err != nil {
			return fmt.Errorf("migration: %w", err)
		}
		pg, err := store.NewPostgresStore(ctx, dbURL)
		if err != nil {
			return fmt.Errorf("postgres store: %w", err)
		}
		if c, ok := any(pg).(interface{ Close() error }); ok {
			defer c.Close()
		}
		st = pg
		logger.Info("using postgres store")
	} else {
		st = store.NewMemoryStore()
		logger.Warn("DATABASE_URL not set; using in-memory store (data will not persist)")
	}

	srv := api.NewServer(st, logger)
	logger.Info("listening", "port", port)
	return srv.Serve(ctx, ":"+port)
}
