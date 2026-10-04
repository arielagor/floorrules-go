//go:build integration

package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/arielagor/floorrules-go/internal/store/pgstore"
	"github.com/arielagor/floorrules-go/internal/store/pgstore/pgtest"
)

// M5 (day-2 review): the migrate Job runs `floorsvc migrate`. On an empty
// database it brings the schema to this binary's version, after which the
// store reports ready; a second run is a no-op.
func TestMigrateCommand_BringsEmptyDatabaseToReady(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewEmptyPool(t)
	dsn := pool.Config().ConnString()
	log := slog.New(slog.DiscardHandler)
	if err := pgstore.New(pool).Ping(ctx); err == nil {
		t.Fatal("empty database reported ready")
	}
	if err := migrate(ctx, dsn, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := pgstore.New(pool).Ping(ctx); err != nil {
		t.Fatalf("not ready after migrate: %v", err)
	}
	if err := migrate(ctx, dsn, log); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if err := migrate(ctx, "postgres://%zz", log); err == nil {
		t.Fatal("bad DSN accepted")
	}
}
