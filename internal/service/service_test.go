package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arielagor/floorrules-go/internal/adapter"
	"github.com/arielagor/floorrules-go/internal/domain"
	"github.com/arielagor/floorrules-go/internal/events"
	"github.com/arielagor/floorrules-go/internal/metrics"
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
	svc := New(st, ssp, Config{Retry: retry, Metrics: m})
	return harness{svc: svc, st: st, ssp: ssp, m: m}
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
	hash := requestHash(p.ID, false)

	// Simulate a worker that claimed the key and is still running.
	if _, _, err := h.st.BeginApply(ctx, domain.ApplyAttempt{IdempotencyKey: "lease-key-01", PlanID: p.ID, RequestHash: hash, Actor: "user:ops"}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "lease-key-01", false); !errors.Is(err, ErrApplyInProgress) {
		t.Fatalf("same key in flight: want ErrApplyInProgress, got %v", err)
	}
	if _, _, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "lease-key-02", false); !errors.Is(err, ErrApplyInProgress) {
		t.Fatalf("other key, plan busy: want ErrApplyInProgress, got %v", err)
	}

	// Ten minutes later the worker is presumed dead; the same key reclaims it.
	h.svc.cfg.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	res, replayed, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "lease-key-01", false)
	if err != nil || replayed || res.Status != domain.PlanApplied {
		t.Fatalf("reclaim = %+v replayed=%v err=%v", res, replayed, err)
	}
}

func TestApply_SurvivesClientDisconnect(t *testing.T) {
	h := newHarness(t)
	h.rule(t, "ctv", "US", 2_000_000)
	p := h.plan(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the HTTP client has gone away
	res, _, err := h.svc.ApplyPlan(ctx, "user:ops", pub, p.ID, "detached-key1", false)
	if err != nil || res.Status != domain.PlanApplied {
		t.Fatalf("apply = %+v, %v; want it to finish despite the cancelled request", res, err)
	}
}

func TestRuleChanged_OutboxToConsumerWithDedupe(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	q := events.NewMemQueue(16, 3, h.svc.cfg.Log)
	handled := make(chan string, 8)
	q.Subscribe(domain.TopicRuleChanged, func(ctx context.Context, m events.Message) error {
		err := h.svc.HandleRuleChanged(ctx, m)
		handled <- m.ID
		return err
	})
	go q.Run(ctx)

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
