package store

import (
	"context"
	"embed"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationsFS embeds every schema migration, applied in filename order.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// MigrationSQL is the concatenated schema (all migrations, in order).
// Kept for tooling that wants a single string.
var MigrationSQL = mustConcatMigrations()

func mustConcatMigrations() string {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		panic(fmt.Sprintf("store: read migrations: %v", err))
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var out string
	for _, n := range names {
		b, err := migrationsFS.ReadFile("migrations/" + n)
		if err != nil {
			panic(fmt.Sprintf("store: read migration %s: %v", n, err))
		}
		out += "\n-- migration: " + n + "\n" + string(b)
	}
	return out
}

// ApplyMigration runs every embedded migration against a fresh database. All
// migrations are idempotent (IF NOT EXISTS / DROP IF EXISTS throughout), so
// it is safe to run on every boot.
func ApplyMigration(ctx context.Context, databaseURL string) error {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("store: connect for migration: %w", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, MigrationSQL); err != nil {
		return fmt.Errorf("store: apply migration: %w", err)
	}
	return nil
}
