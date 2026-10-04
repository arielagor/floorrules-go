//go:build integration

// Integration tests run the shared store contract against a real Postgres.
//
//	docker run -d --name floorrules-pg -e POSTGRES_PASSWORD=test -p 5544:5432 postgres:16
//	TEST_DATABASE_URL=postgres://postgres:test@localhost:5544/postgres?sslmode=disable \
//	  go test -tags integration ./internal/store/pgstore/
package pgstore_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arielagor/floorrules/internal/store"
	"github.com/arielagor/floorrules/internal/store/pgstore"
	"github.com/arielagor/floorrules/internal/store/storetest"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pgstore.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := testPool(t)
	applied, err := pgstore.Migrate(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("second migrate re-applied %v", applied)
	}
}

func TestContract(t *testing.T) {
	pool := testPool(t)
	storetest.Run(t, func(t *testing.T) store.Store {
		_, err := pool.Exec(context.Background(),
			`TRUNCATE rules, plans, apply_attempts, audit_log, outbox, processed_events`)
		if err != nil {
			t.Fatal(err)
		}
		return pgstore.New(pool)
	})
}
