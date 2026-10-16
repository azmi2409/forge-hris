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
//
// Xero integration (optional):
//
//	XERO_CLIENT_ID      Xero developer-app client ID (enables the integration)
//	XERO_CLIENT_SECRET  Xero developer-app client secret
//	XERO_REDIRECT_URL   OAuth redirect URL registered in the Xero app, e.g.
//	                    https://hris.example.com/api/v1/integrations/xero/callback
//	FORGE_HRIS_SECRET_KEY  32-byte key (raw or base64) sealing OAuth tokens;
//	                    required before any Xero connection can be stored
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/azmi2409/forge-hris/api"
	"github.com/azmi2409/forge-hris/integrations/xero"
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
	// The Xero integration is opt-in: set XERO_CLIENT_ID (+ secret and the
	// redirect URL registered in the Xero developer app) to enable the
	// OAuth connect flow and bill posting.
	if clientID := os.Getenv("XERO_CLIENT_ID"); clientID != "" {
		if xs, ok := st.(xero.Store); ok {
			srv.SetXeroIntegration(xero.NewHandler(xs, xero.Config{
				ClientID:     clientID,
				ClientSecret: os.Getenv("XERO_CLIENT_SECRET"),
				RedirectURL:  os.Getenv("XERO_REDIRECT_URL"),
			}).WithLogger(logger))
			logger.Info("xero integration enabled")
		} else {
			logger.Warn("xero env set but the store does not implement xero.Store")
		}
	}
	// The webhook outbox dispatcher is in-process: it drains pending
	// deliveries every 5s until the process shuts down.
	api.StartWebhookDispatcher(ctx, st, logger)
	logger.Info("listening", "port", port)
	return srv.Serve(ctx, ":"+port)
}
