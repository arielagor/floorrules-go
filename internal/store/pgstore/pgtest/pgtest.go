//go:build integration

// Package pgtest gives each integration test its own throwaway Postgres
// database, created from TEST_DATABASE_URL and dropped afterwards. Separate
// databases let several test packages run in parallel (`go test ./...` does)
// without truncating each other's tables.
package pgtest

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arielagor/floorrules-go/internal/id"
	"github.com/arielagor/floorrules-go/internal/store/pgstore"
)

// ErrNoDSNInCI means the integration suite was started in CI without a
// database. That must fail the job: a typo in the CI env would otherwise turn
// the integration job green with zero tests run.
var ErrNoDSNInCI = errors.New("TEST_DATABASE_URL is not set but CI is: the integration suite must not skip in CI")

// DSN decides where integration tests connect. Locally a missing
// TEST_DATABASE_URL skips (skip=true); in CI (CI=true, as GitHub Actions
// sets) it is an error.
func DSN(getenv func(string) string) (dsn string, skip bool, err error) {
	dsn = getenv("TEST_DATABASE_URL")
	switch {
	case dsn != "":
		return dsn, false, nil
	case getenv("CI") == "true":
		return "", false, ErrNoDSNInCI
	default:
		return "", true, nil
	}
}

// NewPool creates a fresh database, migrated to the latest schema, and
// returns a pool connected to it.
func NewPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := NewEmptyPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pgstore.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// NewEmptyPool creates a fresh database with no schema at all.
func NewEmptyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn, skip, err := DSN(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if skip {
		t.Skip("TEST_DATABASE_URL not set (outside CI, integration tests skip)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(ctx)) }()

	name := "ft_" + strings.ReplaceAll(id.New(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Errorf("drop database %s: %v", name, err)
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		if _, err := c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL must be a URL: %v", err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close) // registered after the drop, so it runs first
	return pool
}
