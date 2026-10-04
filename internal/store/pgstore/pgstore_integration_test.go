//go:build integration

// Integration tests run the shared store contract against a real Postgres.
// Each test gets its own database (see pgtest), so packages run in parallel.
//
//	docker run --rm -d --name floorrules-pg -e POSTGRES_PASSWORD=test -p 5544:5432 postgres:16
//	TEST_DATABASE_URL=postgres://postgres:test@localhost:5544/postgres?sslmode=disable \
//	  go test -race -tags integration ./...
//
// Outside CI a missing TEST_DATABASE_URL skips; with CI=true it fails.
package pgstore_test

import (
	"context"
	"testing"

	"github.com/arielagor/floorrules-go/internal/store"
	"github.com/arielagor/floorrules-go/internal/store/pgstore"
	"github.com/arielagor/floorrules-go/internal/store/pgstore/pgtest"
	"github.com/arielagor/floorrules-go/internal/store/storetest"
)

func TestMigrateIsIdempotent(t *testing.T) {
	pool := pgtest.NewPool(t)
	applied, err := pgstore.Migrate(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("second migrate re-applied %v", applied)
	}
}

func TestContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		return pgstore.New(pgtest.NewPool(t))
	})
}
