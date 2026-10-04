// Command floorsvc runs the floor-rules execution service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromOS()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, closeStore, err := openStore(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer closeStore()

	verifier, err := auth.NewHMACVerifier(cfg.HMACSecret, cfg.Issuer, cfg.Audience)
	if err != nil {
		return err
	}

	m := metrics.New()
	m.Describe("http_requests_total", "HTTP requests by route template and status code.")
	m.Describe("ssp_calls_total", "Ad platform calls by operation and outcome (after retries).")
	m.Describe("ssp_retries_total", "Ad platform call retries by operation.")
	m.Describe("apply_total", "Plan applies by resulting status.")
	m.Describe("outbox_published_total", "Outbox events relayed to the queue.")

	// The sample ships only the in-memory SSP; a real adapter for a given ad
	// server implements adapter.SSP and is selected here.
	ssp := adapter.NewMock()
	svc := service.New(st, ssp, service.Config{Log: log, Metrics: m})

	queue := events.NewMemQueue(1024, 5, log)
	queue.Subscribe(domain.TopicRuleChanged, svc.HandleRuleChanged)
	relay := &events.Relay{
		Store: st, Pub: queue, Batch: 100, Lease: 30 * time.Second,
		Interval: cfg.OutboxInterval, Log: log, Metrics: m,
	}

	ready := &atomic.Bool{}
	ready.Store(true)
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
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
	bg.Go(func() { queue.Run(bgCtx) })
	bg.Go(func() { relay.Run(bgCtx) })

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr, "store", cfg.StoreBackend)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
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
	// let in-flight requests (including applies) finish.
	log.Info("shutdown: draining", "drain", cfg.ShutdownDrain)
	ready.Store(false)
	time.Sleep(cfg.ShutdownDrain)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownWait)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown: http server", "err", err)
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
