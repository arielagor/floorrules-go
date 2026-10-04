// Command floorsvc runs the floor-rules execution service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arielagor/floorrules-go/internal/adapter"
	"github.com/arielagor/floorrules-go/internal/auth"
	"github.com/arielagor/floorrules-go/internal/config"
	"github.com/arielagor/floorrules-go/internal/domain"
	"github.com/arielagor/floorrules-go/internal/events"
	"github.com/arielagor/floorrules-go/internal/httpapi"
	"github.com/arielagor/floorrules-go/internal/metrics"
	"github.com/arielagor/floorrules-go/internal/service"
	"github.com/arielagor/floorrules-go/internal/store"
	"github.com/arielagor/floorrules-go/internal/store/memstore"
	"github.com/arielagor/floorrules-go/internal/store/pgstore"
)

func main() {
	if err := mainErr(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func mainErr() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// `floorsvc migrate` applies the schema and exits. It runs as the
	// migrate Job under a role with DDL rights; the service itself does not
	// migrate unless RUN_MIGRATIONS=true (local development).
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
		dsn, err := config.LoadDatabaseURL(os.Getenv, os.ReadFile)
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}
		return migrate(ctx, dsn, log)
	}

	cfg, err := config.FromOS()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)
	return run(ctx, cfg, log, runOpts{})
}

// migrate applies every pending migration and confirms the schema is at
// this binary's latest version.
func migrate(ctx context.Context, dsn string, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return errors.New("database: invalid connection settings") // never echo the DSN
	}
	defer pool.Close()
	applied, err := pgstore.Migrate(ctx, pool)
	if err != nil {
		return fmt.Errorf("migrate: %w (applied before the failure: %v)", err, applied)
	}
	if err := pgstore.SchemaCurrent(ctx, pool); err != nil {
		return err
	}
	latest, _ := pgstore.LatestVersion()
	log.Info("migrations", "applied", applied, "schema", latest)
	return nil
}

// runOpts lets tests inject collaborators. Zero values mean production
// defaults: the store from cfg, the mock SSP, a listener on cfg.ListenAddr.
type runOpts struct {
	store    store.Store
	ssp      adapter.SSP
	listener net.Listener
}

// run serves until ctx is cancelled, then shuts down gracefully.
func run(ctx context.Context, cfg config.Config, log *slog.Logger, o runOpts) error {
	st := o.store
	if st == nil {
		var closeStore func()
		var err error
		st, closeStore, err = openStore(ctx, cfg, log)
		if err != nil {
			return err
		}
		defer closeStore()
	}

	verifier, err := auth.NewHMACVerifier(cfg.HMACSecret, cfg.Issuer, cfg.Audience)
	if err != nil {
		return err
	}

	m := metrics.New()
	m.Describe("http_requests_total", "HTTP requests by route template and status code.")
	m.Describe("ssp_calls_total", "Ad platform calls by operation and outcome (after retries).")
	m.Describe("ssp_retries_total", "Ad platform call retries by operation.")
	m.Describe("apply_total", "Plan applies by resulting status.")
	m.Describe("outbox_published_total", "Outbox events delivered (every consumer succeeded) and marked sent.")
	m.Describe("outbox_delivery_failures_total", "Outbox deliveries that failed and were scheduled for retry or dead-lettered, by topic.")
	m.Describe("outbox_dead_lettered_total", "Outbox events dead-lettered after their last attempt, by topic.")
	m.Describe("outbox_pending", "Outbox events not yet delivered (excluding dead letters).")
	m.Describe("outbox_oldest_pending_seconds", "Age of the oldest undelivered outbox event, 0 when none.")
	m.Describe("outbox_dead_letters", "Dead-lettered outbox events waiting for an operator.")

	// The sample ships only the in-memory SSP; a real adapter for a given ad
	// server implements adapter.SSP and is selected here.
	ssp := o.ssp
	if ssp == nil {
		ssp = adapter.NewMock()
	}
	svc, err := service.New(st, ssp, service.Config{
		Log: log, Metrics: m,
		ApplyTimeout: cfg.ApplyTimeout, RecordTimeout: cfg.RecordTimeout, ApplyLease: cfg.ApplyLease,
	})
	if err != nil {
		return err
	}

	// Consumers run synchronously inside the relay, and a row is marked sent
	// only after they succeed, so the outbox row stays the durable copy until
	// the event is handled. A broker adapter would replace the Dispatcher.
	dispatcher := events.NewDispatcher()
	dispatcher.Subscribe(domain.TopicRuleChanged, svc.HandleRuleChanged)
	relay := &events.Relay{
		Store: st, Pub: dispatcher, Batch: 50, Lease: 2 * time.Minute, Timeout: 10 * time.Second,
		MaxAttempts: 10, BaseBackoff: time.Second, MaxBackoff: 5 * time.Minute,
		Interval: cfg.OutboxInterval, Log: log, Metrics: m,
	}

	ln := o.listener
	if ln == nil {
		var lc net.ListenConfig
		if ln, err = lc.Listen(ctx, "tcp", cfg.ListenAddr); err != nil {
			return err
		}
	}

	ready := &atomic.Bool{}
	ready.Store(true)
	srv := &http.Server{
		Handler:           httpapi.NewHandler(httpapi.Deps{Service: svc, Verifier: verifier, Log: log, Metrics: m, Ready: ready}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      3 * time.Minute, // apply may run up to its 2m budget
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	bgCtx, stopBackground := context.WithCancel(context.Background())
	var bg sync.WaitGroup
	bg.Go(func() { relay.Run(bgCtx) })

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", ln.Addr().String(), "store", cfg.StoreBackend)
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		stopBackground()
		bg.Wait()
		return err
	case <-ctx.Done():
	}

	// Graceful shutdown: fail readiness first so the Service drops this pod
	// from its endpoints, wait for that to propagate, then stop accepting and
	// let in-flight requests finish.
	log.Info("shutdown: draining", "drain", cfg.ShutdownDrain)
	ready.Store(false)
	time.Sleep(cfg.ShutdownDrain)

	// Applies are waited for separately and for longer than ordinary
	// requests: an apply runs detached from its request, so srv.Shutdown
	// returning (or timing out) says nothing about whether it has recorded
	// its result. Exiting before then leaves a partial, unaudited change on
	// the ad platform. The pod's terminationGracePeriodSeconds must cover
	// SHUTDOWN_DRAIN + APPLY_TIMEOUT + RECORD_TIMEOUT + margin.
	shutdownStart := time.Now()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownWait)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown: http server did not finish in SHUTDOWN_WAIT", "err", err)
	}
	applyCtx, cancelApply := context.WithDeadline(context.Background(), shutdownStart.Add(svc.MaxApplyDuration()+2*time.Second))
	defer cancelApply()
	if err := svc.WaitForApplies(applyCtx); err != nil {
		log.Error("shutdown: in-flight apply did not finish; its lease will expire and a later request can reclaim it", "err", err)
	}
	stopBackground()
	bg.Wait()
	log.Info("shutdown: complete")
	return nil
}

func openStore(ctx context.Context, cfg config.Config, log *slog.Logger) (store.Store, func(), error) {
	if cfg.StoreBackend == "memory" {
		log.Warn("using in-memory store: data is lost on restart (local development only)")
		return memstore.New(), func() {}, nil
	}
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(connectCtx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, errors.New("database: invalid connection settings") // never echo the DSN
	}
	if err := pool.Ping(connectCtx); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("database: ping: %w", err)
	}
	if cfg.RunMigrations {
		applied, err := pgstore.Migrate(connectCtx, pool)
		if err != nil {
			pool.Close()
			return nil, nil, err
		}
		log.Info("migrations", "applied", applied)
	}
	return pgstore.New(pool), pool.Close, nil
}
