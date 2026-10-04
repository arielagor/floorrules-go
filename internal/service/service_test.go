package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/arielagor/floorrules-go/internal/adapter"
	"github.com/arielagor/floorrules-go/internal/domain"
	"github.com/arielagor/floorrules-go/internal/events"
	"github.com/arielagor/floorrules-go/internal/metrics"
	"github.com/arielagor/floorrules-go/internal/store"
	"github.com/arielagor/floorrules-go/internal/store/memstore"
)

const pub = "acme-tv"

type harness struct {
	svc *Service
	st  *memstore.Store
	ssp *adapter.Mock
	m   *metrics.Registry
}

func newHarness(t *testing.T) harness {
	t.Helper()
	st := memstore.New()
	ssp := adapter.NewMock()
	m := metrics.New()
	retry := adapter.DefaultRetryPolicy()
	retry.Sleep = func(context.Context, time.Duration) error { return nil } // no real waiting in tests
	svc := mustNew(t, st, ssp, Config{Retry: retry, Metrics: m})
	return harness{svc: svc, st: st, ssp: ssp, m: m}
}

func mustNew(t *testing.T, st store.Store, ssp adapter.SSP, cfg Config) *Service {
	t.Helper()
	svc, err := New(st, ssp, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestNew_RejectsLeaseShorterThanAnApply(t *testing.T) {
	_, err := New(memstore.New(), adapter.NewMock(), Config{ApplyTimeout: 2 * time.Minute, ApplyLease: time.Minute})
	if err == nil {
		t.Fatal("a lease an apply can outlive must be rejected")
	}
}

func seg(device, geo string) domain.Segment {
	return domain.Segment{Device: device, Geo: geo, Genre: domain.Any, DemandPartner: domain.Any}
}

func (h harness) rule(t *testing.T, device, geo string, micros int64) domain.Rule {
	t.Helper()
	r, err := h.svc.CreateRule(context.Background(), "user:ops", domain.Rule{
		PublisherID: pub, Segment: seg(device, geo), FloorMicros: micros,
	})
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	return r
}

func (h harness) plan(t *testing.T) domain.Plan {
	t.Helper()
	p, err := h.svc.CreatePlan(context.Background(), "user:ops", pub)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	return p
}

func TestCreateRule_ValidationAndConflict(t *testing.T) {
	h := newHarness(t)
	_, err := h.svc.CreateRule(context.Background(), "user:ops", domain.Rule{PublisherID: pub, FloorMicros: 1})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want validation error, got %v", err)
	}
	r := h.rule(t, "ctv", "US", 2_000_000)
	if r.ID == "" || r.CreatedBy != "user:ops" || r.Currency != "USD" {
		t.Fatalf("rule = %+v", r)
	}
	_, err = h.svc.CreateRule(context.Background(), "user:ops", domain.Rule{
		PublisherID: pub, Segment: domain.Segment{Device: "CTV", Geo: "us"}, FloorMicros: 3_000_000,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("normalised duplicate segment: want ErrConflict, got %v", err)
	}
}

func TestApply_HappyPathAndReplay(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rule(t, "ctv", "US", 2_000_000)
	h.rule(t, "mobile", "US", 1_000_000)
	p := h.plan(t)
	if len(p.Ops) != 2 {
		t.Fatalf("want 2 ops, got %+v", p.Ops)
	}

	res, replayed, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "apply-key-0001", false)
	if err != nil || replayed || res.Status != domain.PlanApplied {
		t.Fatalf("apply = %+v replayed=%v err=%v", res, replayed, err)
	}
	floors, _ := h.ssp.ListFloors(ctx, pub)
	if len(floors) != 2 {
		t.Fatalf("platform has %d floors, want 2", len(floors))
	}
	setCalls := h.ssp.Calls("SetFloor")

	again, replayed, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "apply-key-0001", false)
	if err != nil || !replayed || again.Status != domain.PlanApplied || len(again.Results) != 2 {
		t.Fatalf("replay = %+v replayed=%v err=%v", again, replayed, err)
	}
	if h.ssp.Calls("SetFloor") != setCalls {
		t.Fatal("replay touched the platform")
	}

	stored, _ := h.svc.GetPlan(ctx, pub, p.ID)
	if stored.Status != domain.PlanApplied {
		t.Fatalf("plan status = %s", stored.Status)
	}
	if _, _, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "apply-key-0002", false); !errors.Is(err, ErrPlanNotPending) {
		t.Fatalf("new key on applied plan: want ErrPlanNotPending, got %v", err)
	}
	if got := h.m.Get("apply_total", "status", "applied"); got != 1 {
		t.Fatalf("apply_total{applied} = %v, want 1", got)
	}

	// A follow-up plan against the converged platform is empty.
	if p2 := h.plan(t); len(p2.Ops) != 0 {
		t.Fatalf("converged plan has ops: %+v", p2.Ops)
	}
}

func TestApply_RefusedOnceShutdownBegins(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rule(t, "ctv", "US", 2_000_000)
	p := h.plan(t)
	if err := h.svc.WaitForApplies(ctx); err != nil { // nothing in flight: returns at once
		t.Fatal(err)
	}
	if _, _, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "apply-key-0001", false); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("apply after shutdown began: want ErrShuttingDown, got %v", err)
	}
	if h.ssp.Calls("SetFloor") != 0 {
		t.Fatal("a refused apply touched the platform")
	}
	if _, err := h.st.GetAttempt(ctx, pub, "apply-key-0001"); err == nil {
		t.Fatal("a refused apply consumed its idempotency key")
	}
}

func TestApply_KeyReuseAndValidation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rule(t, "ctv", "US", 2_000_000)
	p := h.plan(t)
	if _, _, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "short", false); !errors.Is(err, ErrInvalidIdempotencyKey) {
		t.Fatalf("short key: want ErrInvalidIdempotencyKey, got %v", err)
	}
	if _, _, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "bad key with spaces", false); !errors.Is(err, ErrInvalidIdempotencyKey) {
		t.Fatalf("bad chars: want ErrInvalidIdempotencyKey, got %v", err)
	}
	if _, _, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "key-reuse-01", false); err != nil {
		t.Fatal(err)
	}
	// Same key, different request body (ack flag) -> reuse error, not replay.
	if _, _, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "key-reuse-01", true); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("want ErrIdempotencyKeyReused, got %v", err)
	}
	h.rule(t, "ctv", "CA", 2_000_000)
	p2 := h.plan(t)
	if _, _, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p2.ID, "key-reuse-01", false); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("same key, other plan: want ErrIdempotencyKeyReused, got %v", err)
	}
	if _, _, err := h.svc.ApplyPlan(ctx, "user:ops", "other-pub", p2.ID, "key-other-01", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("plan via another publisher: want ErrNotFound, got %v", err)
	}
}

func TestApply_RiskyNeedsAck(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.ssp.Seed(pub, domain.PlatformFloor{Segment: seg("ctv", "US"), FloorMicros: 5_000_000, ManagedBy: domain.ManagedBy})
	h.rule(t, "ctv", "US", 50_000_000) // $5 -> $50: the fat-finger case
	p := h.plan(t)
	if !p.HasRisky() {
		t.Fatalf("plan not flagged risky: %+v", p.Ops)
	}
	if _, _, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "risky-key-01", false); !errors.Is(err, ErrRiskyNotAcknowledged) {
		t.Fatalf("want ErrRiskyNotAcknowledged, got %v", err)
	}
	if h.ssp.Calls("SetFloor") != 0 {
		t.Fatal("unacknowledged risky plan touched the platform")
	}
	// The refused request did not burn the key: the corrected request can use it.
	res, _, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "risky-key-01", true)
	if err != nil || res.Status != domain.PlanApplied {
		t.Fatalf("acknowledged apply = %+v, %v", res, err)
	}
}

func TestApply_RetriesTransientErrors(t *testing.T) {
	h := newHarness(t)
	h.rule(t, "ctv", "US", 2_000_000)
	p := h.plan(t)
	h.ssp.FailNext("SetFloor", &adapter.StatusError{Status: 503}, &adapter.StatusError{Status: 429})
	res, _, err := h.svc.ApplyPlan(context.Background(), "user:ops", pub, p.ID, "retry-key-01", false)
	if err != nil || res.Status != domain.PlanApplied {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	if res.Results[0].Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", res.Results[0].Attempts)
	}
	if got := h.m.Get("ssp_retries_total", "op", "create"); got != 2 {
		t.Fatalf("ssp_retries_total = %v, want 2", got)
	}
}

func TestApply_PartialAndFailed(t *testing.T) {
	h := newHarness(t)
	h.rule(t, "ctv", "CA", 2_000_000)
	h.rule(t, "ctv", "GB", 2_000_000)
	h.rule(t, "ctv", "US", 2_000_000)
	p := h.plan(t)
	// Ops are sorted by segment key: CA, GB, US. Let CA succeed, GB fail permanently.
	h.ssp.FailNext("SetFloor", nil, &adapter.StatusError{Status: 400, Msg: "segment not sellable"})
	res, _, err := h.svc.ApplyPlan(context.Background(), "user:ops", pub, p.ID, "partial-key-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != domain.PlanPartiallyApplied {
		t.Fatalf("status = %s, want partially_applied", res.Status)
	}
	outcomes := []domain.OpOutcome{res.Results[0].Outcome, res.Results[1].Outcome, res.Results[2].Outcome}
	want := []domain.OpOutcome{domain.OutcomeApplied, domain.OutcomeFailed, domain.OutcomeSkipped}
	for i := range want {
		if outcomes[i] != want[i] {
			t.Fatalf("outcomes = %v, want %v", outcomes, want)
		}
	}
	if res.Results[1].Attempts != 1 {
		t.Fatalf("a 400 was retried: %d attempts", res.Results[1].Attempts)
	}

	// A fresh plan converges on what is left, and a first-op failure is "failed".
	p2 := h.plan(t)
	if len(p2.Ops) != 2 {
		t.Fatalf("re-plan should hold the 2 remaining creates, got %+v", p2.Ops)
	}
	h.ssp.FailNext("SetFloor", &adapter.StatusError{Status: 403})
	res2, _, _ := h.svc.ApplyPlan(context.Background(), "user:ops", pub, p2.ID, "failed-key-01", false)
	if res2.Status != domain.PlanFailed {
		t.Fatalf("status = %s, want failed", res2.Status)
	}
}

func TestApply_DriftMakesPlanStale(t *testing.T) {
	h := newHarness(t)
	h.rule(t, "ctv", "US", 2_000_000)
	p := h.plan(t)
	// Someone edits the ad server by hand after the plan was reviewed.
	h.ssp.Seed(pub, domain.PlatformFloor{Segment: seg("desktop", "US"), FloorMicros: 900_000, ManagedBy: "human"})
	res, _, err := h.svc.ApplyPlan(context.Background(), "user:ops", pub, p.ID, "drift-key-01", false)
	if err != nil || res.Status != domain.PlanStale {
		t.Fatalf("apply = %+v, %v; want stale", res, err)
	}
	if h.ssp.Calls("SetFloor") != 0 {
		t.Fatal("stale plan touched the platform")
	}
}

func TestApply_PlatformDownAtStart(t *testing.T) {
	h := newHarness(t)
	h.rule(t, "ctv", "US", 2_000_000)
	p := h.plan(t)
	fail := make([]error, 10)
	for i := range fail {
		fail[i] = &adapter.StatusError{Status: 502}
	}
	h.ssp.FailNext("ListFloors", fail...)
	res, _, err := h.svc.ApplyPlan(context.Background(), "user:ops", pub, p.ID, "down-key-001", false)
	if err != nil || res.Status != domain.PlanFailed || res.Reason == "" {
		t.Fatalf("apply = %+v, %v; want failed with reason", res, err)
	}
	if _, err := h.svc.CreatePlan(context.Background(), "user:ops", pub); !errors.Is(err, ErrPlatformUnavailable) {
		t.Fatalf("CreatePlan with platform down: want ErrPlatformUnavailable, got %v", err)
	}
}

func TestApply_InProgressAndLeaseReclaim(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rule(t, "ctv", "US", 2_000_000)
	p := h.plan(t)
	hash := requestHash(pub, p.ID, false)

	// Simulate a worker that claimed the key and is still running.
	if _, _, err := h.st.BeginApply(ctx, domain.ApplyAttempt{PublisherID: pub, IdempotencyKey: "lease-key-01", PlanID: p.ID, RequestHash: hash, Actor: "user:ops"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "lease-key-01", false); !errors.Is(err, ErrApplyInProgress) {
		t.Fatalf("same key in flight: want ErrApplyInProgress, got %v", err)
	}
	if _, _, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "lease-key-02", false); !errors.Is(err, ErrApplyInProgress) {
		t.Fatalf("other key, plan busy: want ErrApplyInProgress, got %v", err)
	}

	// Ten minutes later (on the store's clock, which measures leases) the
	// worker is presumed dead; the same key reclaims it.
	h.st.SetClock(func() time.Time { return time.Now().Add(10 * time.Minute) })
	res, replayed, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "lease-key-01", false)
	if err != nil || replayed || res.Status != domain.PlanApplied {
		t.Fatalf("reclaim = %+v replayed=%v err=%v", res, replayed, err)
	}
}

// M3 (day-2 review): idempotency keys belong to a publisher. A caller scoped
// to publisher A must not read B's stored result by naming B's plan and key,
// and two publishers that pick the same key must not collide.
func TestApply_IdempotencyKeysAreTenantScoped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const other = "rival-tv"
	const key = "apply-0001-shared"

	// Publisher B applies its own plan with the key.
	if _, err := h.svc.CreateRule(ctx, "user:b", domain.Rule{PublisherID: other, Segment: seg("ctv", "US"), FloorMicros: 3_000_000}); err != nil {
		t.Fatal(err)
	}
	planB, err := h.svc.CreatePlan(ctx, "user:b", other)
	if err != nil {
		t.Fatal(err)
	}
	if res, _, err := h.svc.ApplyPlan(ctx, "user:b", other, planB.ID, key, false); err != nil || res.Status != domain.PlanApplied {
		t.Fatalf("B's apply: %+v %v", res, err)
	}

	// A caller scoped to publisher A replays B's plan and key.
	leaked, replayed, err := h.svc.ApplyPlan(ctx, "user:a", pub, planB.ID, key, false)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-publisher replay: want ErrNotFound, got err=%v replayed=%v result=%+v", err, replayed, leaked)
	}

	// Publisher A uses the same key for its own plan: no collision.
	h.rule(t, "ctv", "US", 2_000_000)
	planA := h.plan(t)
	if res, replayed, err := h.svc.ApplyPlan(ctx, "user:a", pub, planA.ID, key, false); err != nil || replayed || res.Status != domain.PlanApplied {
		t.Errorf("same key, other publisher: %+v replayed=%v err=%v; want a fresh apply", res, replayed, err)
	}
}

// interleavingStore runs `before` once, just before the BeginApply for key,
// to place another request's whole apply inside this one's check-then-act
// window.
type interleavingStore struct {
	*memstore.Store
	key    string
	before func()
	once   sync.Once
}

func (s *interleavingStore) BeginApply(ctx context.Context, a domain.ApplyAttempt, lease time.Duration) (domain.ApplyAttempt, bool, error) {
	if a.IdempotencyKey == s.key {
		s.once.Do(s.before)
	}
	return s.Store.BeginApply(ctx, a, lease)
}

// M2 (day-2 review): the double click with a fresh key. B checks the plan
// is pending; A applies it in full; B then claims and runs it. B must be
// refused at the claim, and A's "applied" must not be overwritten.
func TestApply_SecondKeyAfterFirstFinishedIsRefused(t *testing.T) {
	mem := memstore.New()
	ist := &interleavingStore{Store: mem, key: "click-two-0001"}
	ssp := adapter.NewMock()
	retry := adapter.DefaultRetryPolicy()
	retry.Sleep = func(context.Context, time.Duration) error { return nil }
	svc := mustNew(t, ist, ssp, Config{Retry: retry})
	h := harness{svc: svc, st: mem, ssp: ssp}
	ctx := context.Background()
	h.rule(t, "ctv", "US", 2_000_000)
	p := h.plan(t)

	ist.before = func() {
		if res, _, err := svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "click-one-0001", false); err != nil || res.Status != domain.PlanApplied {
			t.Errorf("first click: %+v %v", res, err)
		}
	}
	res, _, err := svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "click-two-0001", false)
	if !errors.Is(err, ErrPlanNotPending) {
		t.Errorf("second click: want ErrPlanNotPending, got %v (result %s)", err, res.Status)
	}
	if ssp.Calls("SetFloor") != 1 {
		t.Errorf("SetFloor calls = %d, want 1 (the second click must not touch the platform)", ssp.Calls("SetFloor"))
	}
	if got, _ := svc.GetPlan(ctx, pub, p.ID); got.Status != domain.PlanApplied {
		t.Errorf("plan status = %s, want applied: the second click overwrote it", got.Status)
	}
	applies := 0
	entries, _ := mem.ListAudit(ctx, pub, 50)
	for _, e := range entries {
		if e.Action == "plan.apply" {
			applies++
		}
	}
	if applies != 1 {
		t.Errorf("plan.apply audit rows = %d, want 1", applies)
	}
}

// disconnectingSSP cancels the HTTP request's context on the first write,
// i.e. the client goes away after the apply has claimed its key.
type disconnectingSSP struct {
	*adapter.Mock
	cancel context.CancelFunc
}

func (d disconnectingSSP) SetFloor(ctx context.Context, pub string, f domain.PlatformFloor) error {
	d.cancel()
	return d.Mock.SetFloor(ctx, pub, f)
}

// A disconnect after the claim must not abandon the apply halfway. (Before the
// day-2 review this test cancelled the request before calling ApplyPlan and
// passed only because the in-memory fake ignored its context; against
// Postgres, pre-claim reads correctly fail on a dead request.)
func TestApply_SurvivesClientDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := memstore.New()
	retry := adapter.DefaultRetryPolicy()
	retry.Sleep = func(context.Context, time.Duration) error { return nil }
	svc := mustNew(t, st, disconnectingSSP{Mock: adapter.NewMock(), cancel: cancel}, Config{Retry: retry})
	h := harness{svc: svc, st: st}
	h.rule(t, "ctv", "US", 2_000_000)
	h.rule(t, "ctv", "CA", 2_000_000)
	p := h.plan(t)
	res, _, err := svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "detached-key1", false)
	if err != nil || res.Status != domain.PlanApplied {
		t.Fatalf("apply = %+v, %v; want it to finish despite the cancelled request", res, err)
	}
	if ctx.Err() == nil {
		t.Fatal("test bug: the request context was never cancelled")
	}
}

// hangingSSP is a platform that answers ListFloors but never answers
// SetFloor: each write blocks until the caller's deadline. It is the most
// common real failure, a slow SSP that eats the whole apply budget.
type hangingSSP struct{ *adapter.Mock }

func (hangingSSP) SetFloor(ctx context.Context, _ string, _ domain.PlatformFloor) error {
	<-ctx.Done()
	return ctx.Err()
}

// H1 (day-2 review): the result used to be written on the apply's own,
// already-expired context, so a slow SSP left no audit row, no event and an
// attempt stuck in_progress.
func TestApply_SlowPlatformStillRecordsResult(t *testing.T) {
	st := memstore.New()
	m := metrics.New()
	retry := adapter.DefaultRetryPolicy()
	retry.PerAttempt = 0 // one attempt runs to the overall deadline
	retry.Sleep = func(context.Context, time.Duration) error { return nil }
	svc := mustNew(t, st, hangingSSP{adapter.NewMock()}, Config{Retry: retry, Metrics: m, ApplyTimeout: 50 * time.Millisecond})
	h := harness{svc: svc, st: st, m: m}
	h.rule(t, "ctv", "US", 2_000_000)
	p := h.plan(t)

	res, _, err := svc.ApplyPlan(context.Background(), "user:ops", pub, p.ID, "slow-ssp-key-1", false)
	if err != nil {
		t.Fatalf("apply returned %v; the outcome must be recorded, not lost", err)
	}
	if res.Status != domain.PlanFailed {
		t.Fatalf("status = %s, want failed", res.Status)
	}
	a, err := st.GetAttempt(context.Background(), pub, "slow-ssp-key-1")
	if err != nil || a.Status != domain.AttemptFailed || a.Result == nil {
		t.Fatalf("attempt = %+v, %v; want failed with a stored result", a, err)
	}
	audit, _ := st.ListAudit(context.Background(), pub, 1)
	if len(audit) != 1 || audit[0].Action != "plan.apply" {
		t.Fatalf("newest audit entry = %+v, want plan.apply", audit)
	}
}

func TestRuleChanged_OutboxToConsumerWithDedupe(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	q := events.NewDispatcher()
	handled := make(chan string, 8)
	q.Subscribe(domain.TopicRuleChanged, func(ctx context.Context, m events.Message) error {
		err := h.svc.HandleRuleChanged(ctx, m)
		handled <- m.ID
		return err
	})

	h.rule(t, "ctv", "US", 2_000_000)
	relay := &events.Relay{Store: h.st, Pub: q, Batch: 10, Lease: time.Minute, Log: h.svc.cfg.Log, Metrics: h.m}
	n, err := relay.RunOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("relay published %d, %v", n, err)
	}
	eventID := waitFor(t, handled)
	if got := h.m.Get("auto_plans_total"); got != 1 {
		t.Fatalf("auto_plans_total = %v, want 1", got)
	}

	// Redeliver the same event, as a broker may: no second plan.
	dup := events.Message{ID: eventID, Topic: domain.TopicRuleChanged, Payload: []byte(`{"publisher_id":"acme-tv"}`)}
	if err := q.Publish(ctx, dup); err != nil {
		t.Fatal(err)
	}
	waitFor(t, handled)
	if got := h.m.Get("events_deduplicated_total", "consumer", AutoPlannerConsumer); got != 1 {
		t.Fatalf("events_deduplicated_total = %v, want 1", got)
	}

	// Malformed events are acked, not redelivered forever.
	if err := h.svc.HandleRuleChanged(ctx, events.Message{ID: "x", Payload: []byte(`{`)}); err != nil {
		t.Fatalf("malformed event should be dropped, got %v", err)
	}
}

// Found by the container smoke run: an event that arrives after its rule was
// already applied used to produce an empty "proposed" plan. No drift means
// nothing to review, so no plan.
func TestRuleChanged_NoDriftProposesNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.rule(t, "ctv", "US", 2_000_000)
	h.ssp.Seed(pub, domain.PlatformFloor{Segment: seg("ctv", "US"), FloorMicros: 2_000_000, ManagedBy: domain.ManagedBy})

	msg := events.Message{ID: "evt-no-drift", Topic: domain.TopicRuleChanged, Payload: []byte(`{"publisher_id":"acme-tv"}`)}
	if err := h.svc.HandleRuleChanged(ctx, msg); err != nil {
		t.Fatal(err)
	}
	if got := h.m.Get("auto_plans_total"); got != 0 {
		t.Fatalf("auto_plans_total = %v, want 0 for a no-drift event", got)
	}
	if got := h.m.Get("auto_plans_skipped_total", "reason", "no_drift"); got != 1 {
		t.Fatalf("auto_plans_skipped_total = %v, want 1", got)
	}
}

func waitFor(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for handler")
		return ""
	}
}
