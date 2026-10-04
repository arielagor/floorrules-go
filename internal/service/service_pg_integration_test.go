//go:build integration

// Concurrency tests against real Postgres: two Service instances (two pods)
// share one database and one ad platform. Run under -race, as CI does:
//
//	go test -race -tags integration ./internal/service/
package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arielagor/floorrules-go/internal/adapter"
	"github.com/arielagor/floorrules-go/internal/domain"
	"github.com/arielagor/floorrules-go/internal/metrics"
	"github.com/arielagor/floorrules-go/internal/store/pgstore"
	"github.com/arielagor/floorrules-go/internal/store/pgstore/pgtest"
)

// stallingSSP is a worker whose platform call hangs past its lease: a long
// GC pause, a network partition, a stuck TCP connection. It ignores ctx on
// purpose, as a paused process would. When released it fails the write, so
// a late, unfenced record would overwrite a good result with a bad one.
type stallingSSP struct {
	*adapter.Mock
	entered, release chan struct{}
	once             sync.Once
}

func (s *stallingSSP) SetFloor(context.Context, string, domain.PlatformFloor) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return &adapter.StatusError{Status: 400, Msg: "worker A's late write"}
}

func noSleepRetry() adapter.RetryPolicy {
	r := adapter.DefaultRetryPolicy()
	r.Sleep = func(context.Context, time.Duration) error { return nil }
	return r
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// M1 (day-2 review). Worker A claims an apply and stalls. Its lease expires,
// worker B reclaims the same key and applies the plan. Then A wakes up and
// tries to record its stale result. Fencing must reject A's write: the
// attempt, the plan, the audit log and the outbox all reflect B alone.
func TestTwoWorkers_LeaseTakeoverFencesTheStaleWorker(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := pgstore.New(pool)
	platform := adapter.NewMock()
	ctx := context.Background()

	stall := &stallingSSP{Mock: platform, entered: make(chan struct{}), release: make(chan struct{})}
	mA, mB := metrics.New(), metrics.New()
	workerA := mustNew(t, st, stall, Config{Retry: noSleepRetry(), Metrics: mA})
	workerB := mustNew(t, st, platform, Config{Retry: noSleepRetry(), Metrics: mB})

	if _, err := workerB.CreateRule(ctx, "user:ops", domain.Rule{
		PublisherID: pub, Segment: seg("ctv", "US"), FloorMicros: 2_500_000,
	}); err != nil {
		t.Fatal(err)
	}
	plan, err := workerB.CreatePlan(ctx, "user:ops", pub)
	if err != nil {
		t.Fatal(err)
	}
	const key = "takeover-key-0001"

	// Worker A claims the key and hangs inside the platform call.
	aErr := make(chan error, 1)
	go func() {
		_, _, err := workerA.ApplyPlan(ctx, "user:worker-a", pub, plan.ID, key, false)
		aErr <- err
	}()
	select {
	case <-stall.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("worker A never reached the platform")
	}

	// A's lease runs out (backdated in the database, whose clock is the one
	// the lease is measured on).
	if _, err := pool.Exec(ctx, `UPDATE apply_attempts SET started_at = now() - interval '1 hour'
		WHERE idempotency_key = $1`, key); err != nil {
		t.Fatal(err)
	}

	// Worker B (the client retrying against another pod) takes over.
	res, replayed, err := workerB.ApplyPlan(ctx, "user:worker-b", pub, plan.ID, key, false)
	if err != nil || replayed || res.Status != domain.PlanApplied {
		t.Fatalf("worker B: %+v replayed=%v err=%v", res, replayed, err)
	}

	// A wakes up and tries to record its failed result.
	close(stall.release)
	select {
	case err = <-aErr:
	case <-time.After(10 * time.Second):
		t.Fatal("worker A never returned")
	}
	if !errors.Is(err, ErrLeaseLost) {
		t.Errorf("worker A: want ErrLeaseLost (its late result fenced off), got %v", err)
	}
	if got := mA.Get("apply_lease_lost_total"); got != 1 {
		t.Errorf("apply_lease_lost_total on worker A = %v, want 1", got)
	}

	// The record reflects B alone.
	plan2, err := st.GetPlan(ctx, pub, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan2.Status != domain.PlanApplied {
		t.Errorf("plan status = %s, want applied (B's result)", plan2.Status)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM apply_attempts
		WHERE idempotency_key = $1 AND status = 'succeeded' AND actor = 'user:worker-b'`, key); n != 1 {
		t.Errorf("attempt does not hold B's successful result")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM audit_log WHERE action = 'plan.apply'`); n != 1 {
		t.Errorf("plan.apply audit rows = %d, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM audit_log
		WHERE action = 'plan.apply' AND actor = 'user:worker-b'`); n != 1 {
		t.Errorf("the plan.apply audit row is not B's")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM outbox WHERE topic = $1`, domain.TopicPlanApplied); n != 1 {
		t.Errorf("plan.applied events = %d, want 1", n)
	}
	floors, _ := platform.ListFloors(ctx, pub)
	if len(floors) != 1 || floors[0].FloorMicros != 2_500_000 {
		t.Errorf("platform floors = %+v", floors)
	}
}

// H1 against Postgres: a platform slow enough to use the whole apply budget
// still gets its outcome recorded, with the audit row and event.
func TestSlowPlatformRecordsResultInPostgres(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := pgstore.New(pool)
	ctx := context.Background()
	retry := noSleepRetry()
	retry.PerAttempt = 0
	svc := mustNew(t, st, &hangingSSP{Mock: adapter.NewMock()}, Config{
		Retry: retry, Metrics: metrics.New(), ApplyTimeout: 100 * time.Millisecond,
	})
	if _, err := svc.CreateRule(ctx, "user:ops", domain.Rule{
		PublisherID: pub, Segment: seg("ctv", "US"), FloorMicros: 2_500_000,
	}); err != nil {
		t.Fatal(err)
	}
	plan, err := svc.CreatePlan(ctx, "user:ops", pub)
	if err != nil {
		t.Fatal(err)
	}
	res, _, err := svc.ApplyPlan(ctx, "user:ops", pub, plan.ID, "slow-key-0001", false)
	if err != nil {
		t.Fatalf("apply returned %v; the outcome must be recorded, not lost", err)
	}
	if res.Status != domain.PlanFailed {
		t.Fatalf("status = %s, want failed", res.Status)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM apply_attempts WHERE status = 'failed' AND result IS NOT NULL`); n != 1 {
		t.Errorf("attempt not recorded as failed")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM audit_log WHERE action = 'plan.apply'`); n != 1 {
		t.Errorf("plan.apply audit rows = %d, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM outbox WHERE topic = $1`, domain.TopicPlanApplied); n != 1 {
		t.Errorf("plan.applied events = %d, want 1", n)
	}
}
