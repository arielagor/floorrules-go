package config

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

const secret32 = "abcdefghijklmnopqrstuvwxyz012345"

func env(m map[string]string) Getenv { return func(k string) string { return m[k] } }

func noFiles(string) ([]byte, error) { return nil, errors.New("no such file") }

func TestLoad_DefaultsMemory(t *testing.T) {
	c, err := Load(env(map[string]string{
		"STORE_BACKEND": "memory", "AUTH_HMAC_SECRET": secret32, "AUTH_ISSUER": "dev",
	}), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || c.Audience != "floorrules" || !c.RunMigrations || c.ShutdownDrain != 5*time.Second {
		t.Fatalf("defaults wrong: %+v", c)
	}
}

func TestLoad_SecretFilesTakePrecedence(t *testing.T) {
	files := map[string]string{
		"/var/run/secrets/hmac": secret32 + "\n",
		"/var/run/secrets/db":   "postgres://u:p@db/floor\n",
	}
	read := func(p string) ([]byte, error) {
		if v, ok := files[p]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("missing")
	}
	c, err := Load(env(map[string]string{
		"AUTH_HMAC_SECRET_FILE": "/var/run/secrets/hmac", "AUTH_HMAC_SECRET": "ignored-because-file-wins-xxxxxxxx",
		"DATABASE_URL_FILE": "/var/run/secrets/db", "AUTH_ISSUER": "https://issuer",
		"LOG_LEVEL": "debug", "SHUTDOWN_DRAIN": "2s",
	}), read)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.HMACSecret) != secret32 || c.DatabaseURL != "postgres://u:p@db/floor" {
		t.Fatalf("secret files not used or not trimmed: %+v", c)
	}
	if c.LogLevel != slog.LevelDebug || c.ShutdownDrain != 2*time.Second {
		t.Fatalf("parsed values wrong: %+v", c)
	}
}

func TestLoad_ReportsAllProblemsWithoutLeakingValues(t *testing.T) {
	_, err := Load(env(map[string]string{
		"AUTH_HMAC_SECRET": "short-secret-value", "STORE_BACKEND": "postgres",
		"LOG_LEVEL": "loud", "SHUTDOWN_WAIT": "soon",
	}), noFiles)
	if err == nil {
		t.Fatal("want errors")
	}
	msg := err.Error()
	for _, want := range []string{"AUTH_HMAC_SECRET", "AUTH_ISSUER", "DATABASE_URL", "LOG_LEVEL", "SHUTDOWN_WAIT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %s in %q", want, msg)
		}
	}
	if strings.Contains(msg, "short-secret-value") {
		t.Fatal("error message leaked the secret")
	}
}

func TestLoad_UnreadableSecretFileAndBadBackend(t *testing.T) {
	_, err := Load(env(map[string]string{
		"AUTH_HMAC_SECRET_FILE": "/nope", "AUTH_ISSUER": "x", "STORE_BACKEND": "sqlite",
	}), noFiles)
	if err == nil || !strings.Contains(err.Error(), "AUTH_HMAC_SECRET_FILE") || !strings.Contains(err.Error(), "STORE_BACKEND") {
		t.Fatalf("got %v", err)
	}
}
