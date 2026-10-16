// Command forge-hris-mcp runs the forge-hris component 4 MCP server: the
// JSON-RPC 2.0 agent surface over the same store the REST API uses.
//
// Transports:
//   - stdio (default): newline-delimited JSON-RPC on stdin/stdout, for
//     Claude Desktop and Claude Code. The bearer token comes from
//     FORGE_HRIS_MCP_TOKEN or the token field of initialize params.
//   - HTTP: --addr (e.g. :8081) serves POST /mcp (JSON-RPC) and
//     GET /mcp (SSE). Every request carries Authorization: Bearer.
//
// Storage is selected by DATABASE_URL, mirroring forge-hris-api:
//   - set: Postgres via the pgx adapter (schema auto-applied on boot).
//   - unset: in-memory store (data lost on restart; fine for dev).
//
// Configuration via environment:
//
//	FORGE_HRIS_MCP_TOKEN  bearer token for stdio mode (optional when the
//	                      client passes token in initialize params)
//	DATABASE_URL          postgres connection string (optional)
//	MCP_ADDR              HTTP listen address fallback when --addr is empty
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/azmi2409/forge-hris/mcp"
	"github.com/azmi2409/forge-hris/store"
)

func main() {
	// Logs go to stderr: stdout is the JSON-RPC channel in stdio mode.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	stdio := flag.Bool("stdio", true, "use the stdio transport (default)")
	addr := flag.String("addr", "", "HTTP listen address, e.g. :8081 (when set, serves HTTP instead of stdio)")
	flag.Parse()

	if *addr == "" {
		*addr = os.Getenv("MCP_ADDR")
	}
	if !*stdio && *addr == "" {
		return fmt.Errorf("--stdio=false requires --addr")
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

	srv := mcp.NewServer(st, logger)

	if *addr != "" {
		logger.Info("serving MCP over HTTP", "addr", *addr)
		httpSrv := &http.Server{
			Addr:              *addr,
			Handler:           srv.HTTPHandler(),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
		errCh := make(chan error, 1)
		go func() { errCh <- httpSrv.ListenAndServe() }()
		select {
		case <-ctx.Done():
			shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return httpSrv.Shutdown(shutCtx)
		case err := <-errCh:
			return err
		}
	}

	logger.Info("serving MCP over stdio")
	return srv.ServeStdio(ctx, os.Stdin, os.Stdout, os.Getenv("FORGE_HRIS_MCP_TOKEN"))
}
