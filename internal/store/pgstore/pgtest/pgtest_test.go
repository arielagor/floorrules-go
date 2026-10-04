//go:build integration

package pgtest

import (
	"errors"
	"testing"
)

func TestDSN_FailsInCISkipsLocally(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	if _, _, err := DSN(env(map[string]string{"CI": "true"})); !errors.Is(err, ErrNoDSNInCI) {
		t.Fatalf("CI without TEST_DATABASE_URL: want ErrNoDSNInCI, got %v", err)
	}
	if _, skip, err := DSN(env(map[string]string{})); err != nil || !skip {
		t.Fatalf("local without TEST_DATABASE_URL: want skip, got skip=%v err=%v", skip, err)
	}
	dsn, skip, err := DSN(env(map[string]string{"CI": "true", "TEST_DATABASE_URL": "postgres://x"}))
	if err != nil || skip || dsn != "postgres://x" {
		t.Fatalf("with TEST_DATABASE_URL: got %q skip=%v err=%v", dsn, skip, err)
	}
}
