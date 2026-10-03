// Package migrate — минимальный раннер миграций: файлы sql/NNN_*.sql применяются
// по порядку, каждая в своей транзакции; применённые пишутся в schema_migrations.
package migrate

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed sql/*.sql
var files embed.FS

func Up(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	names, err := fs.Glob(files, "sql/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)

	var applied []string
	for _, name := range names {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&exists); err != nil {
			return applied, err
		}
		if exists {
			continue
		}
		sql, err := files.ReadFile(name)
		if err != nil {
			return applied, err
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return applied, err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("%s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return applied, err
		}
		if err := tx.Commit(ctx); err != nil {
			return applied, err
		}
		applied = append(applied, name)
	}
	return applied, nil
}
