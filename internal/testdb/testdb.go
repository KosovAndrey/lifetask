// Package testdb — чистая БД для интеграционных тестов. Адрес — TEST_DATABASE_URL;
// без него тесты пропускаются.
package testdb

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/lifeplan/internal/migrate"
)

func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL не задан")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Up(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
