// Package config loads settings from the environment. Secrets may be given
// as a file path (*_FILE, e.g. a Kubernetes secret volume or a Secret
// Manager CSI mount) which takes precedence over the plain variable. Nothing
// is ever read from a file in the repository.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config is the process configuration.
type Config struct {
	ListenAddr string
	// MetricsAddr serves /metrics on its own listener, apart from the API
	// the load balancer exposes (default :9090). run() skips it when empty,
	// which only a Config built in code can be.
	MetricsAddr    string
	StoreBackend   string // "postgres" or "memory"
	DatabaseURL    string
	RunMigrations  bool
	HMACSecret     []byte
	Issuer         string
	Audience       string
	LogLevel       slog.Level
	ShutdownDrain  time.Duration
	ShutdownWait   time.Duration
	OutboxInterval time.Duration
	// ApplyTimeout bounds executing one plan; RecordTimeout bounds writing
	// its result. ApplyLease is how long an in-progress attempt is trusted
	// before another request may reclaim it, so it must exceed both.
	ApplyTimeout  time.Duration
	RecordTimeout time.Duration
	ApplyLease    time.Duration
}

// Getenv matches os.Getenv; tests pass a map lookup.
type Getenv func(string) string

// ReadFile matches os.ReadFile.
type ReadFile func(string) ([]byte, error)

// Load reads and validates configuration. It returns every problem at once.
func Load(getenv Getenv, readFile ReadFile) (Config, error) {
	var errs []error
	c := Config{
		ListenAddr:   or(getenv("LISTEN_ADDR"), ":8080"),
		MetricsAddr:  or(getenv("METRICS_ADDR"), ":9090"),
		StoreBackend: or(getenv("STORE_BACKEND"), "postgres"),
		// Off by default: schema changes run as `floorsvc migrate` (the
		// migrate Job) under a role with DDL rights, which the service's
		// runtime role does not have. Set true only for local development.
		RunMigrations:  getenv("RUN_MIGRATIONS") == "true",
		Issuer:         getenv("AUTH_ISSUER"),
		Audience:       or(getenv("AUTH_AUDIENCE"), "floorrules"),
		ShutdownDrain:  5 * time.Second,
		ShutdownWait:   20 * time.Second,
		OutboxInterval: time.Second,
		ApplyTimeout:   2 * time.Minute,
		RecordTimeout:  10 * time.Second,
		ApplyLease:     5 * time.Minute,
	}

	secret, err := secretValue(getenv, readFile, "AUTH_HMAC_SECRET")
	if err != nil {
		errs = append(errs, err)
	}
	c.HMACSecret = []byte(secret)
	if len(c.HMACSecret) < 32 {
		errs = append(errs, errors.New("AUTH_HMAC_SECRET (or AUTH_HMAC_SECRET_FILE) must be at least 32 bytes"))
	}
	if c.Issuer == "" {
		errs = append(errs, errors.New("AUTH_ISSUER is required"))
	}

	switch c.StoreBackend {
	case "postgres":
		c.DatabaseURL, err = secretValue(getenv, readFile, "DATABASE_URL")
		if err != nil {
			errs = append(errs, err)
		} else if c.DatabaseURL == "" {
			errs = append(errs, errors.New("DATABASE_URL (or DATABASE_URL_FILE) is required for STORE_BACKEND=postgres"))
		} else if err := requireVerifiedTLS(getenv, c.DatabaseURL); err != nil {
			errs = append(errs, err)
		}
	case "memory":
	default:
		errs = append(errs, fmt.Errorf("STORE_BACKEND must be postgres or memory, got %q", c.StoreBackend))
	}

	if v := getenv("LOG_LEVEL"); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
		}
	}
	for _, d := range []struct {
		name string
		dst  *time.Duration
	}{
		{"SHUTDOWN_DRAIN", &c.ShutdownDrain}, {"SHUTDOWN_WAIT", &c.ShutdownWait}, {"OUTBOX_INTERVAL", &c.OutboxInterval},
		{"APPLY_TIMEOUT", &c.ApplyTimeout}, {"RECORD_TIMEOUT", &c.RecordTimeout}, {"APPLY_LEASE", &c.ApplyLease},
	} {
		if v := getenv(d.name); v != "" {
			parsed, err := time.ParseDuration(v)
			if err != nil || parsed < 0 {
				errs = append(errs, fmt.Errorf("%s: invalid duration %q", d.name, v))
				continue
			}
			*d.dst = parsed
		}
	}
	if c.ApplyTimeout <= 0 || c.RecordTimeout <= 0 {
		errs = append(errs, errors.New("APPLY_TIMEOUT and RECORD_TIMEOUT must be positive"))
	}
	if c.ApplyLease <= c.ApplyTimeout+c.RecordTimeout {
		// A shorter lease lets a second worker reclaim an attempt that is
		// still running, and both would write to the platform.
		errs = append(errs, fmt.Errorf("APPLY_LEASE (%s) must exceed APPLY_TIMEOUT + RECORD_TIMEOUT (%s)",
			c.ApplyLease, c.ApplyTimeout+c.RecordTimeout))
	}
	return c, errors.Join(errs...)
}

// LoadDatabaseURL reads only DATABASE_URL (or DATABASE_URL_FILE). The
// migrate command uses it: it needs the migrator's DSN and nothing else.
func LoadDatabaseURL(getenv Getenv, readFile ReadFile) (string, error) {
	dsn, err := secretValue(getenv, readFile, "DATABASE_URL")
	if err != nil {
		return "", err
	}
	if dsn == "" {
		return "", errors.New("DATABASE_URL (or DATABASE_URL_FILE) is required")
	}
	if err := requireVerifiedTLS(getenv, dsn); err != nil {
		return "", err
	}
	return dsn, nil
}

// requireVerifiedTLS rejects a DSN that would talk to Postgres without
// checking the server certificate. pgx defaults to sslmode=prefer, which
// encrypts but accepts any certificate, and `require` is no better: both
// lose to anyone who can answer on the database address. Only verify-ca
// (with sslrootcert, the Cloud SQL server CA) and verify-full pass.
// DATABASE_TLS_UNVERIFIED_OK=true is the explicit opt-out for a local
// database; nothing in deploy/ sets it. Errors never include the DSN.
func requireVerifiedTLS(getenv Getenv, dsn string) error {
	if getenv("DATABASE_TLS_UNVERIFIED_OK") == "true" {
		return nil
	}
	mode := sslMode(dsn)
	if mode == "" {
		mode = getenv("PGSSLMODE")
	}
	switch mode {
	case "verify-ca", "verify-full":
		return nil
	case "":
		mode = "unset, so prefer"
	}
	return fmt.Errorf("DATABASE_URL: sslmode is %s; use verify-ca or verify-full (or DATABASE_TLS_UNVERIFIED_OK=true for a local database)", mode)
}

// sslMode extracts sslmode from a URL or key=value connection string.
func sslMode(dsn string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return ""
		}
		return u.Query().Get("sslmode")
	}
	for _, field := range strings.Fields(dsn) {
		if v, ok := strings.CutPrefix(field, "sslmode="); ok {
			return strings.Trim(v, `'"`)
		}
	}
	return ""
}

// secretValue prefers NAME_FILE over NAME. Error messages name the variable,
// never the value.
func secretValue(getenv Getenv, readFile ReadFile, name string) (string, error) {
	if path := getenv(name + "_FILE"); path != "" {
		b, err := readFile(path)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: cannot read secret file", name)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return getenv(name), nil
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// FromOS loads from the real environment and filesystem.
func FromOS() (Config, error) { return Load(os.Getenv, os.ReadFile) }
