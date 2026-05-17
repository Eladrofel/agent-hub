package store

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Eladrofel/agent-hub/gateway/db"
)

// Migrations are embedded (see gateway/db/embed.go) so the binary is
// self-contained — no runtime dependency on the source tree or a mounted
// volume. The compose mount of gateway/db/migrations into
// /docker-entrypoint-initdb.d is a parallel convenience for first-boot
// bootstrap; this runner handles any cluster that already exists, including
// v0.1.x+ migrations added after first boot.

const schemaMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version text PRIMARY KEY,
  applied_at timestamptz NOT NULL DEFAULT now()
);
`

// Migrate applies every embedded migration file whose version is not already
// recorded in schema_migrations. Idempotent; safe to call on every boot.
// Each migration is applied in its own transaction so a partial failure
// leaves the cluster at a consistent recorded version.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.Pool.Exec(ctx, schemaMigrationsTable); err != nil {
		return fmt.Errorf("ensure schema_migrations table: %w", err)
	}

	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return fmt.Errorf("load applied versions: %w", err)
	}

	files, err := listMigrations()
	if err != nil {
		return fmt.Errorf("list embedded migrations: %w", err)
	}

	for _, m := range files {
		if applied[m.version] {
			continue
		}
		if err := s.applyOne(ctx, m); err != nil {
			return fmt.Errorf("apply %s: %w", m.version, err)
		}
	}
	return nil
}

func (s *Store) appliedVersions(ctx context.Context) (map[string]bool, error) {
	rows, err := s.Pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

type migration struct {
	version string // e.g., "001_init"
	sql     string
}

func listMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(db.Migrations, "migrations")
	if err != nil {
		return nil, err
	}

	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := db.Migrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{
			version: strings.TrimSuffix(e.Name(), ".sql"),
			sql:     string(raw),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func (s *Store) applyOne(ctx context.Context, m migration) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations(version) VALUES ($1)`,
			m.version)
		return err
	})
}
