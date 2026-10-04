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
	"errors"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arielagor/floorrules-go/internal/domain"
	"github.com/arielagor/floorrules-go/internal/id"
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

// M5 (day-2 review): /readyz pinged the pool and nothing else, so on a
// database the migrate Job had not reached yet, pods went Ready and served
// `relation "rules" does not exist`. The store is ready only at the schema
// this binary was built for.
func TestPing_RequiresLatestSchema(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewEmptyPool(t)
	s := pgstore.New(pool)
	if err := s.Ping(ctx); err == nil {
		t.Fatal("Ping succeeded on a database with no schema; pods would go Ready and fail every request")
	}
	if _, err := pgstore.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping after migrate: %v", err)
	}
	// A database one migration behind (the Job has not run for this
	// release yet) is not ready either.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = (SELECT max(version) FROM schema_migrations)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(ctx); err == nil {
		t.Fatal("Ping succeeded on a database one migration behind")
	}
}

// M5: the migrate Job runs as floorsvc_migrator; the service runs as
// floorsvc_app, which can use every table but change none of them.
// deploy/sql/roles.sql is the grant script operators run; this test runs
// the same file.
func TestRoles_RuntimeRoleHasNoDDL(t *testing.T) {
	ctx := context.Background()
	admin := pgtest.NewEmptyPool(t)
	for _, role := range []string{"floorsvc_migrator", "floorsvc_app"} {
		// Roles are cluster-wide; create them once and reuse.
		if _, err := admin.Exec(ctx, `DO $$ BEGIN CREATE ROLE `+role+` LOGIN PASSWORD 'test-only';
			EXCEPTION WHEN duplicate_object THEN NULL; END $$`); err != nil {
			t.Fatal(err)
		}
	}
	grants, err := os.ReadFile("../../../deploy/sql/roles.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, string(grants)); err != nil {
		t.Fatalf("roles.sql: %v", err)
	}
	as := func(role string) *pgxpool.Pool {
		u, err := url.Parse(admin.Config().ConnString())
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(role, "test-only")
		p, err := pgxpool.New(ctx, u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}

	if _, err := pgstore.Migrate(ctx, as("floorsvc_migrator")); err != nil {
		t.Fatalf("migrate as floorsvc_migrator: %v", err)
	}
	app := as("floorsvc_app")
	s := pgstore.New(app)
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("runtime role cannot read the schema version: %v", err)
	}
	r := domain.Rule{
		ID: id.New(), PublisherID: "pub-a", CreatedBy: "user:test", Currency: "USD", FloorMicros: 2_000_000,
		Segment: domain.Segment{Device: "ctv", Geo: "US", Genre: domain.Any, DemandPartner: domain.Any},
	}
	if _, err := s.CreateRule(ctx, r); err != nil {
		t.Fatalf("runtime role cannot write: %v", err)
	}
	for _, ddl := range []string{
		`CREATE TABLE sneaky (i int)`,
		`ALTER TABLE rules ADD COLUMN sneaky int`,
		`DROP TABLE outbox`,
	} {
		_, err := app.Exec(ctx, ddl)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s as floorsvc_app: err = %v, want insufficient_privilege", ddl, err)
		}
	}
}

func TestContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		return pgstore.New(pgtest.NewPool(t))
	})
}
