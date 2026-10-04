// Package storetest is the contract test suite every store.Store must pass.
// The in-memory fake runs it in every `go test`; Postgres runs it under the
// `integration` build tag. Keeping one suite is what lets unit tests trust the
// fake: it is held to the same behaviour as the real database.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/arielagor/floorrules-go/internal/domain"
	"github.com/arielagor/floorrules-go/internal/id"
	"github.com/arielagor/floorrules-go/internal/store"
)

// Factory returns a fresh, empty store for one subtest.
type Factory func(t *testing.T) store.Store

// Run executes the full contract suite.
func Run(t *testing.T, newStore Factory) {
	t.Run("RuleLifecycle", func(t *testing.T) { testRuleLifecycle(t, newStore(t)) })
	t.Run("PlanScopedToPublisher", func(t *testing.T) { testPlanScoped(t, newStore(t)) })
	t.Run("ApplyIdempotency", func(t *testing.T) { testApplyIdempotency(t, newStore(t)) })
	t.Run("ApplyLeaseReclaim", func(t *testing.T) { testLeaseReclaim(t, newStore(t)) })
	t.Run("ConcurrentBeginApply", func(t *testing.T) { testConcurrentBegin(t, newStore(t)) })
	t.Run("ClaimRequiresPendingPlan", func(t *testing.T) { testClaimRequiresPending(t, newStore(t)) })
	t.Run("IdempotencyKeysScopedToPublisher", func(t *testing.T) { testKeysScoped(t, newStore(t)) })
	t.Run("AuditAndOutboxAtomicWithChange", func(t *testing.T) { testAuditOutbox(t, newStore(t)) })
	t.Run("EventDedupe", func(t *testing.T) { testEventDedupe(t, newStore(t)) })
	t.Run("OutboxRedelivery", func(t *testing.T) { testOutboxRedelivery(t, newStore(t)) })
}

func newRule(pub, geo string) domain.Rule {
	return domain.Rule{
		ID: id.New(), PublisherID: pub, CreatedBy: "user:test", Currency: "USD", FloorMicros: 2_000_000,
		Segment: domain.Segment{Device: "ctv", Geo: geo, Genre: domain.Any, DemandPartner: domain.Any},
	}
}

func newPlan(pub string) domain.Plan {
	return domain.Plan{
		ID: id.New(), PublisherID: pub, Status: domain.PlanPending, CreatedBy: "user:test",
		BaseFingerprint: "fp",
		Ops:             []domain.Op{{Kind: domain.OpCreate, Segment: domain.Segment{Device: "ctv", Geo: "US", Genre: "*", DemandPartner: "*"}, ToMicros: 1_000_000}},
	}
}

func testRuleLifecycle(t *testing.T, s store.Store) {
	ctx := context.Background()
	r, err := s.CreateRule(ctx, newRule("pub-a", "US"))
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	if r.Status != domain.RuleActive || r.Version != 1 || r.CreatedAt.IsZero() {
		t.Fatalf("created rule = %+v", r)
	}
	if _, err := s.CreateRule(ctx, newRule("pub-a", "US")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate active segment: want ErrConflict, got %v", err)
	}
	if _, err := s.CreateRule(ctx, newRule("pub-b", "US")); err != nil {
		t.Fatalf("same segment, other publisher should be allowed: %v", err)
	}
	if _, err := s.DisableRule(ctx, "pub-b", r.ID, "user:x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("disable via wrong publisher: want ErrNotFound, got %v", err)
	}
	d, err := s.DisableRule(ctx, "pub-a", r.ID, "user:test")
	if err != nil || d.Status != domain.RuleDisabled || d.Version != 2 {
		t.Fatalf("DisableRule = %+v, %v", d, err)
	}
	if _, err := s.DisableRule(ctx, "pub-a", r.ID, "user:test"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second disable: want ErrNotFound, got %v", err)
	}
	if _, err := s.CreateRule(ctx, newRule("pub-a", "US")); err != nil {
		t.Fatalf("re-create after disable: %v", err)
	}
	rules, err := s.ListRules(ctx, "pub-a")
	if err != nil || len(rules) != 2 {
		t.Fatalf("ListRules = %d rules, %v; want 2", len(rules), err)
	}
	if rules[0].ID != r.ID || rules[0].Status != domain.RuleDisabled {
		t.Fatalf("want oldest (disabled) rule first, got %+v", rules[0])
	}
	if rules[1].Segment != r.Segment {
		t.Fatalf("segment did not round-trip: %+v", rules[1].Segment)
	}
	empty, err := s.ListRules(ctx, "nobody")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("ListRules(unknown) = %v, %v; want empty non-nil slice", empty, err)
	}
}

func testPlanScoped(t *testing.T, s store.Store) {
	ctx := context.Background()
	p := newPlan("pub-a")
	if err := s.CreatePlan(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPlan(ctx, "pub-a", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.PlanPending || len(got.Ops) != 1 || got.Ops[0] != p.Ops[0] || got.BaseFingerprint != "fp" {
		t.Fatalf("plan did not round-trip: %+v", got)
	}
	if _, err := s.GetPlan(ctx, "pub-b", p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-publisher read: want ErrNotFound, got %v", err)
	}
	if _, err := s.GetPlan(ctx, "pub-a", id.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing plan: want ErrNotFound, got %v", err)
	}
}

func attempt(key, planID string) domain.ApplyAttempt {
	return domain.ApplyAttempt{PublisherID: "pub-a", IdempotencyKey: key, PlanID: planID, RequestHash: "h1", Actor: "user:test"}
}

func testApplyIdempotency(t *testing.T, s store.Store) {
	ctx := context.Background()
	p := newPlan("pub-a")
	if err := s.CreatePlan(ctx, p); err != nil {
		t.Fatal(err)
	}
	const lease = time.Hour

	if _, err := s.GetAttempt(ctx, "pub-a", "key-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetAttempt before begin: want ErrNotFound, got %v", err)
	}
	a, created, err := s.BeginApply(ctx, attempt("key-1", p.ID), lease)
	if err != nil || !created || a.Status != domain.AttemptInProgress || a.Token == "" {
		t.Fatalf("first BeginApply = %+v, %v, %v; want created with a token", a, created, err)
	}
	again, created, err := s.BeginApply(ctx, attempt("key-1", p.ID), lease)
	if err != nil || created || again.Status != domain.AttemptInProgress {
		t.Fatalf("repeat BeginApply = %+v, %v, %v; want existing in-progress, not created", again, created, err)
	}
	if again.Token != "" {
		t.Fatal("a caller that did not claim the attempt was handed its token")
	}
	if _, _, err := s.BeginApply(ctx, attempt("key-2", p.ID), lease); !errors.Is(err, store.ErrPlanBusy) {
		t.Fatalf("second key on busy plan: want ErrPlanBusy, got %v", err)
	}

	result := domain.ApplyResult{PlanID: p.ID, Status: domain.PlanApplied, Results: []domain.OpResult{
		{Op: p.Ops[0], Outcome: domain.OutcomeApplied, Attempts: 2},
	}}
	if err := s.FinishApply(ctx, a, result); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishApply(ctx, a, result); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("finishing a finished attempt: want ErrLeaseLost, got %v", err)
	}
	done, created, err := s.BeginApply(ctx, attempt("key-1", p.ID), lease)
	if err != nil || created || done.Status != domain.AttemptSucceeded || done.Result == nil {
		t.Fatalf("replay after finish = %+v, %v, %v", done, created, err)
	}
	fetched, err := s.GetAttempt(ctx, "pub-a", "key-1")
	if err != nil || fetched.Status != domain.AttemptSucceeded || fetched.FinishedAt == nil {
		t.Fatalf("GetAttempt after finish = %+v, %v", fetched, err)
	}
	want, _ := json.Marshal(result)
	got, _ := json.Marshal(done.Result)
	if string(want) != string(got) {
		t.Fatalf("stored result differs:\n got %s\nwant %s", got, want)
	}
	plan, _ := s.GetPlan(ctx, "pub-a", p.ID)
	if plan.Status != domain.PlanApplied {
		t.Fatalf("plan status = %s, want applied", plan.Status)
	}
	if err := s.FinishApply(ctx, attempt("no-such-key", p.ID), result); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("finish unknown key: want ErrNotFound, got %v", err)
	}
}

// testLeaseReclaim covers reclaim and fencing (day-2 review M1): once an
// expired attempt is reclaimed, the previous holder's token is dead and its
// late result changes nothing.
func testLeaseReclaim(t *testing.T, s store.Store) {
	ctx := context.Background()
	p := newPlan("pub-a")
	if err := s.CreatePlan(ctx, p); err != nil {
		t.Fatal(err)
	}
	first, _, err := s.BeginApply(ctx, attempt("key-1", p.ID), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, _ := s.BeginApply(ctx, attempt("key-1", p.ID), time.Hour); created {
		t.Fatal("reclaimed an attempt whose lease is still live")
	}
	// Let the attempt age past a short lease (both stores measure age on
	// their own clock, so the test waits rather than moving a clock).
	time.Sleep(50 * time.Millisecond)
	const short = 10 * time.Millisecond

	// A different request body under the same key is never reclaimed.
	other := attempt("key-1", p.ID)
	other.RequestHash = "h2"
	if _, created, _ := s.BeginApply(ctx, other, short); created {
		t.Fatal("reclaimed an attempt whose request hash differs")
	}
	// Same request, lease expired: the attempt counts as abandoned.
	second := attempt("key-1", p.ID)
	second.Actor = "user:second"
	b, created, err := s.BeginApply(ctx, second, short)
	if err != nil || !created || b.Status != domain.AttemptInProgress {
		t.Fatalf("reclaim = %+v, %v, %v", b, created, err)
	}
	if b.Token == "" || b.Token == first.Token {
		t.Fatalf("reclaim must issue a new token: first=%q second=%q", first.Token, b.Token)
	}

	// The first holder wakes up: its result is refused and changes nothing.
	failed := domain.ApplyResult{PlanID: p.ID, Status: domain.PlanFailed}
	if err := s.FinishApply(ctx, first, failed); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("stale holder's FinishApply: want ErrLeaseLost, got %v", err)
	}
	if got, _ := s.GetPlan(ctx, "pub-a", p.ID); got.Status != domain.PlanPending {
		t.Fatalf("stale holder changed the plan to %s", got.Status)
	}
	if entries, _ := s.ListAudit(ctx, "pub-a", 10); len(entries) != 1 || entries[0].Action != "plan.create" {
		t.Fatalf("stale holder wrote audit entries: %+v", entries)
	}

	// The current holder records normally.
	if err := s.FinishApply(ctx, b, domain.ApplyResult{PlanID: p.ID, Status: domain.PlanApplied}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetAttempt(ctx, "pub-a", "key-1")
	if got.Status != domain.AttemptSucceeded || got.Actor != "user:second" {
		t.Fatalf("attempt = %+v, want succeeded by user:second", got)
	}
}

func testConcurrentBegin(t *testing.T, s store.Store) {
	ctx := context.Background()
	p := newPlan("pub-a")
	if err := s.CreatePlan(ctx, p); err != nil {
		t.Fatal(err)
	}
	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	createdCount, busy := 0, 0
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := "same-key"
			if i%2 == 1 {
				key = "other-" + id.New()
			}
			_, created, err := s.BeginApply(ctx, attempt(key, p.ID), time.Hour)
			mu.Lock()
			defer mu.Unlock()
			if errors.Is(err, store.ErrPlanBusy) {
				busy++
				return
			}
			if err != nil {
				t.Errorf("BeginApply: %v", err)
			}
			if created {
				createdCount++
			}
		}()
	}
	wg.Wait()
	if createdCount != 1 {
		t.Fatalf("%d attempts created concurrently for one plan, want exactly 1 (busy=%d)", createdCount, busy)
	}
}

// testClaimRequiresPendingPlan (day-2 review M2): the pending check belongs to
// the claim's transaction. Key A applies the plan; a fresh key B must then be
// refused, and the plan stays applied.
func testClaimRequiresPending(t *testing.T, s store.Store) {
	ctx := context.Background()
	p := newPlan("pub-a")
	if err := s.CreatePlan(ctx, p); err != nil {
		t.Fatal(err)
	}
	a, _, err := s.BeginApply(ctx, attempt("key-a", p.ID), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishApply(ctx, a, domain.ApplyResult{PlanID: p.ID, Status: domain.PlanApplied}); err != nil {
		t.Fatal(err)
	}
	b := attempt("key-b", p.ID)
	b.RequestHash = "h-b"
	if _, created, err := s.BeginApply(ctx, b, time.Hour); created || !errors.Is(err, store.ErrPlanNotPending) {
		t.Fatalf("fresh key on an applied plan: created=%v err=%v; want ErrPlanNotPending", created, err)
	}
	if _, _, err := s.BeginApply(ctx, attempt("key-c", id.New()), time.Hour); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim on a missing plan: want ErrNotFound, got %v", err)
	}
	if got, _ := s.GetPlan(ctx, "pub-a", p.ID); got.Status != domain.PlanApplied {
		t.Fatalf("plan status = %s, want applied", got.Status)
	}
	// The finished key still replays.
	if done, created, err := s.BeginApply(ctx, attempt("key-a", p.ID), time.Hour); err != nil || created || done.Result == nil {
		t.Fatalf("replay of the finished key = %+v, %v, %v", done, created, err)
	}
}

// testKeysScoped (day-2 review M3): the same key under two publishers is two
// attempts, and no lookup or claim crosses a publisher boundary.
func testKeysScoped(t *testing.T, s store.Store) {
	ctx := context.Background()
	pa, pb := newPlan("pub-a"), newPlan("pub-b")
	for _, p := range []domain.Plan{pa, pb} {
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	a := attempt("shared-key", pa.ID)
	b := attempt("shared-key", pb.ID)
	b.PublisherID = "pub-b"
	ca, created, err := s.BeginApply(ctx, a, time.Hour)
	if err != nil || !created {
		t.Fatalf("pub-a claim = %v, %v", created, err)
	}
	cb, created, err := s.BeginApply(ctx, b, time.Hour)
	if err != nil || !created {
		t.Fatalf("same key under pub-b = %v, %v; want its own attempt", created, err)
	}
	if err := s.FinishApply(ctx, ca, domain.ApplyResult{PlanID: pa.ID, Status: domain.PlanApplied}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetAttempt(ctx, "pub-b", "shared-key"); err != nil || got.PlanID != pb.ID || got.Status != domain.AttemptInProgress {
		t.Fatalf("pub-b's attempt = %+v, %v; pub-a's finish leaked into it", got, err)
	}
	if err := s.FinishApply(ctx, cb, domain.ApplyResult{PlanID: pb.ID, Status: domain.PlanFailed}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAttempt(ctx, "pub-c", "shared-key"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lookup under another publisher: want ErrNotFound, got %v", err)
	}
	// A claim naming another publisher's plan finds nothing.
	cross := attempt("cross-key", pb.ID)
	if _, _, err := s.BeginApply(ctx, cross, time.Hour); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pub-a claiming pub-b's plan: want ErrNotFound, got %v", err)
	}
}

func testAuditOutbox(t *testing.T, s store.Store) {
	ctx := context.Background()
	r, err := s.CreateRule(ctx, newRule("pub-a", "US"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRule(ctx, newRule("pub-a", "US")); err == nil {
		t.Fatal("want conflict")
	}
	if _, err := s.DisableRule(ctx, "pub-a", r.ID, "user:ops"); err != nil {
		t.Fatal(err)
	}
	p := newPlan("pub-a")
	if err := s.CreatePlan(ctx, p); err != nil {
		t.Fatal(err)
	}
	claim, _, err := s.BeginApply(ctx, attempt("k", p.ID), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishApply(ctx, claim, domain.ApplyResult{PlanID: p.ID, Status: domain.PlanApplied}); err != nil {
		t.Fatal(err)
	}

	entries, err := s.ListAudit(ctx, "pub-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range entries {
		actions = append(actions, e.Action)
	}
	wantActions := []string{"plan.apply", "plan.create", "rule.disable", "rule.create"}
	if len(actions) != len(wantActions) {
		t.Fatalf("audit actions = %v, want %v (the failed duplicate must leave no trace)", actions, wantActions)
	}
	for i := range wantActions {
		if actions[i] != wantActions[i] {
			t.Fatalf("audit actions = %v, want newest-first %v", actions, wantActions)
		}
	}
	if limited, _ := s.ListAudit(ctx, "pub-a", 2); len(limited) != 2 {
		t.Fatalf("limit ignored: %d entries", len(limited))
	}
	if other, _ := s.ListAudit(ctx, "pub-b", 10); len(other) != 0 {
		t.Fatalf("audit leaked across publishers: %v", other)
	}

	evs, err := s.ClaimOutbox(ctx, 10, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var topics []string
	for _, e := range evs {
		topics = append(topics, e.Topic)
	}
	wantTopics := []string{domain.TopicRuleChanged, domain.TopicRuleChanged, domain.TopicPlanApplied}
	if len(topics) != 3 || topics[0] != wantTopics[0] || topics[1] != wantTopics[1] || topics[2] != wantTopics[2] {
		t.Fatalf("outbox topics = %v, want %v", topics, wantTopics)
	}
	var payload domain.RuleChangedPayload
	if err := json.Unmarshal(evs[0].Payload, &payload); err != nil || payload.RuleID != r.ID || payload.Change != "created" {
		t.Fatalf("rule.changed payload = %s (%v)", evs[0].Payload, err)
	}
	if again, _ := s.ClaimOutbox(ctx, 10, time.Hour); len(again) != 0 {
		t.Fatalf("leased events were handed out twice: %d", len(again))
	}
	if err := s.MarkOutboxSent(ctx, []string{evs[0].ID}); err != nil {
		t.Fatal(err)
	}
}

func testEventDedupe(t *testing.T, s store.Store) {
	ctx := context.Background()
	evID := id.New()
	ok, err := s.CreatePlanForEvent(ctx, "planner", evID, newPlan("pub-a"))
	if err != nil || !ok {
		t.Fatalf("first delivery = %v, %v", ok, err)
	}
	dup := newPlan("pub-a")
	ok, err = s.CreatePlanForEvent(ctx, "planner", evID, dup)
	if err != nil || ok {
		t.Fatalf("duplicate delivery = %v, %v; want ignored", ok, err)
	}
	if _, err := s.GetPlan(ctx, "pub-a", dup.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("duplicate delivery stored a plan: %v", err)
	}
	if ok, _ := s.CreatePlanForEvent(ctx, "other-consumer", evID, newPlan("pub-a")); !ok {
		t.Fatal("dedupe must be per consumer")
	}
}

// testOutboxRedelivery: an event whose lease expired without being marked
// sent (the relay crashed after publishing) is handed out again.
func testOutboxRedelivery(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.CreateRule(ctx, newRule("pub-a", "US")); err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimOutbox(ctx, 10, 0)
	if err != nil || len(first) != 1 {
		t.Fatalf("claim = %d events, %v", len(first), err)
	}
	second, err := s.ClaimOutbox(ctx, 10, time.Hour)
	if err != nil || len(second) != 1 || second[0].ID != first[0].ID {
		t.Fatalf("expired lease not redelivered: %v, %v", second, err)
	}
	if err := s.MarkOutboxSent(ctx, []string{first[0].ID}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if third, _ := s.ClaimOutbox(ctx, 10, 0); len(third) != 0 {
		t.Fatalf("sent event redelivered: %v", third)
	}
}
