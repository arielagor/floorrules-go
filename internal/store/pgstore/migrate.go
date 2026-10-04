package pgstore

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID is an arbitrary constant for pg_advisory_lock, so that when
// several replicas start at once only one runs migrations.
const migrationLockID int64 = 0x666c6f6f72 // "floor"

// LatestVersion is the newest embedded migration: the schema this binary
// expects.
func LatestVersion() (string, error) {
	names, err := migrationNames()
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", errors.New("no embedded migrations")
	}
	return versionOf(names[len(names)-1]), nil
}

func migrationNames() ([]string, error) {
	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

func versionOf(name string) string {
	return strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
}

// SchemaCurrent returns nil if the database has the latest embedded
// migration applied, and an error naming what is missing otherwise.
func SchemaCurrent(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) error {
	want, err := LatestVersion()
	if err != nil {
		return err
	}
	var have bool
	err = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, want).Scan(&have)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
		return fmt.Errorf("schema not migrated: want %s, schema_migrations does not exist", want)
	}
	if err != nil {
		return err
	}
	if !have {
		return fmt.Errorf("schema not migrated: want %s", want)
	}
	return nil
}

// Migrate applies every embedded migration not yet recorded in
// schema_migrations, each in its own transaction, in filename order.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return nil, fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		// Use a fresh context: the caller's may already be cancelled, and a
		// session-level advisory lock must be released explicitly.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}

	names, err := migrationNames()
	if err != nil {
		return nil, err
	}

	var applied []string
	for _, name := range names {
		version := versionOf(name)
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&exists); err != nil {
			return applied, err
		}
		if exists {
			continue
		}
		body, err := migrationFS.ReadFile(name)
		if err != nil {
			return applied, err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version)
			return err
		})
		if err != nil {
			return applied, fmt.Errorf("migration %s: %w", version, err)
		}
		applied = append(applied, version)
	}
	return applied, nil
}
