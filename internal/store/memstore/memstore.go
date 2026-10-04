// Package memstore is an in-memory store.Store. It backs unit tests and the
// no-database local mode; it passes the same contract suite as Postgres.
package memstore

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/arielagor/floorrules-go/internal/domain"
	"github.com/arielagor/floorrules-go/internal/id"
	"github.com/arielagor/floorrules-go/internal/store"
)

type outboxRow struct {
	ev           domain.Event
	claimedUntil time.Time
	sent         bool
}

// Store is safe for concurrent use; one mutex makes every method atomic,
// which is the in-memory equivalent of a transaction.
type Store struct {
	mu        sync.Mutex
	now       func() time.Time
	rules     map[string]domain.Rule
	ruleSeq   map[string]int
	plans     map[string]domain.Plan
	attempts  map[string]domain.ApplyAttempt
	audit     []domain.AuditEntry
	outbox    []*outboxRow
	processed map[string]bool
}

var _ store.Store = (*Store)(nil)

// New returns an empty store.
func New() *Store {
	return &Store{
		now:       time.Now,
		rules:     map[string]domain.Rule{},
		ruleSeq:   map[string]int{},
		plans:     map[string]domain.Plan{},
		attempts:  map[string]domain.ApplyAttempt{},
		processed: map[string]bool{},
	}
}

// Ping implements store.Store.
func (s *Store) Ping(ctx context.Context) error { return ctx.Err() }

func (s *Store) auditLocked(actor, pub, action, entity string, detail any) {
	raw, _ := json.Marshal(detail)
	s.audit = append(s.audit, domain.AuditEntry{
		ID: int64(len(s.audit) + 1), At: s.now().UTC(), Actor: actor, PublisherID: pub,
		Action: action, EntityID: entity, Detail: raw,
	})
}

func (s *Store) emitLocked(topic string, payload any) {
	raw, _ := json.Marshal(payload)
	s.outbox = append(s.outbox, &outboxRow{ev: domain.Event{
		ID: id.New(), Topic: topic, Payload: raw, CreatedAt: s.now().UTC(),
	}})
}

// CreateRule implements store.Store.
func (s *Store) CreateRule(ctx context.Context, r domain.Rule) (domain.Rule, error) {
	if err := ctx.Err(); err != nil { // honour cancellation like a real database driver
		return domain.Rule{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.rules {
		if existing.PublisherID == r.PublisherID && existing.Status == domain.RuleActive &&
			existing.Segment == r.Segment {
			return domain.Rule{}, store.ErrConflict
		}
	}
	now := s.now().UTC()
	r.Status = domain.RuleActive
	r.Version = 1
	r.CreatedAt, r.UpdatedAt = now, now
	s.rules[r.ID] = r
	s.ruleSeq[r.ID] = len(s.ruleSeq) + 1
	s.auditLocked(r.CreatedBy, r.PublisherID, "rule.create", r.ID, r)
	s.emitLocked(domain.TopicRuleChanged, domain.RuleChangedPayload{PublisherID: r.PublisherID, RuleID: r.ID, Change: "created"})
	return r, nil
}

// DisableRule implements store.Store.
func (s *Store) DisableRule(ctx context.Context, publisherID, ruleID, actor string) (domain.Rule, error) {
	if err := ctx.Err(); err != nil { // honour cancellation like a real database driver
		return domain.Rule{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rules[ruleID]
	if !ok || r.PublisherID != publisherID || r.Status != domain.RuleActive {
		return domain.Rule{}, store.ErrNotFound
	}
	r.Status = domain.RuleDisabled
	r.Version++
	r.UpdatedAt = s.now().UTC()
	s.rules[ruleID] = r
	s.auditLocked(actor, publisherID, "rule.disable", ruleID, map[string]any{"version": r.Version})
	s.emitLocked(domain.TopicRuleChanged, domain.RuleChangedPayload{PublisherID: publisherID, RuleID: ruleID, Change: "disabled"})
	return r, nil
}

// ListRules implements store.Store. Results are ordered by creation time.
func (s *Store) ListRules(ctx context.Context, publisherID string) ([]domain.Rule, error) {
	if err := ctx.Err(); err != nil { // honour cancellation like a real database driver
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []domain.Rule{}
	for _, r := range s.rules {
		if r.PublisherID == publisherID {
			out = append(out, r)
		}
	}
	// Insertion order, like the Postgres seq column. Timestamps alone can tie.
	sort.Slice(out, func(i, j int) bool { return s.ruleSeq[out[i].ID] < s.ruleSeq[out[j].ID] })
	return out, nil
}

// CreatePlan implements store.Store.
func (s *Store) CreatePlan(ctx context.Context, p domain.Plan) error {
	if err := ctx.Err(); err != nil { // honour cancellation like a real database driver
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createPlanLocked(p)
	return nil
}

func (s *Store) createPlanLocked(p domain.Plan) {
	if p.CreatedAt.IsZero() {
		p.CreatedAt = s.now().UTC()
	}
	if p.Ops == nil {
		p.Ops = []domain.Op{}
	}
	s.plans[p.ID] = p
	s.auditLocked(p.CreatedBy, p.PublisherID, "plan.create", p.ID, map[string]any{"ops": len(p.Ops)})
}

// GetPlan implements store.Store.
func (s *Store) GetPlan(ctx context.Context, publisherID, planID string) (domain.Plan, error) {
	if err := ctx.Err(); err != nil { // honour cancellation like a real database driver
		return domain.Plan{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[planID]
	if !ok || p.PublisherID != publisherID {
		return domain.Plan{}, store.ErrNotFound
	}
	return p, nil
}

// attemptKey scopes an idempotency key to its publisher, like the Postgres
// primary key (publisher_id, idempotency_key).
func attemptKey(publisherID, key string) string { return publisherID + "\x00" + key }

// GetAttempt implements store.Store.
func (s *Store) GetAttempt(ctx context.Context, publisherID, key string) (domain.ApplyAttempt, error) {
	if err := ctx.Err(); err != nil { // honour cancellation like a real database driver
		return domain.ApplyAttempt{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[attemptKey(publisherID, key)]
	if !ok {
		return domain.ApplyAttempt{}, store.ErrNotFound
	}
	return a, nil
}

// BeginApply implements store.Store. The lease is measured on the store's
// clock (SetClock), like Postgres measures it on now().
func (s *Store) BeginApply(ctx context.Context, a domain.ApplyAttempt, lease time.Duration) (domain.ApplyAttempt, bool, error) {
	if err := ctx.Err(); err != nil { // honour cancellation like a real database driver
		return domain.ApplyAttempt{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	plan, ok := s.plans[a.PlanID]
	if !ok || plan.PublisherID != a.PublisherID {
		return domain.ApplyAttempt{}, false, store.ErrNotFound
	}
	if existing, ok := s.attempts[attemptKey(a.PublisherID, a.IdempotencyKey)]; ok {
		if existing.Status == domain.AttemptInProgress && existing.RequestHash == a.RequestHash &&
			existing.StartedAt.Before(now.Add(-lease)) {
			existing.StartedAt = now
			existing.Actor = a.Actor
			existing.Token = id.New() // fences off the previous holder
			s.attempts[attemptKey(a.PublisherID, a.IdempotencyKey)] = existing
			return existing, true, nil
		}
		existing.Token = "" // only the claimant gets a usable token
		return existing, false, nil
	}
	if plan.Status != domain.PlanPending {
		return domain.ApplyAttempt{}, false, store.ErrPlanNotPending
	}
	for _, other := range s.attempts {
		if other.PlanID == a.PlanID && other.Status == domain.AttemptInProgress {
			return domain.ApplyAttempt{}, false, store.ErrPlanBusy
		}
	}
	a.Status = domain.AttemptInProgress
	a.StartedAt = now
	a.Result = nil
	a.FinishedAt = nil
	a.Token = id.New()
	s.attempts[attemptKey(a.PublisherID, a.IdempotencyKey)] = a
	return a, true, nil
}

// FinishApply implements store.Store.
func (s *Store) FinishApply(ctx context.Context, claim domain.ApplyAttempt, result domain.ApplyResult) error {
	if err := ctx.Err(); err != nil { // honour cancellation like a real database driver
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	publisherID, key, actor := claim.PublisherID, claim.IdempotencyKey, claim.Actor
	a, ok := s.attempts[attemptKey(publisherID, key)]
	if !ok {
		return store.ErrNotFound
	}
	if a.Status != domain.AttemptInProgress || claim.Token == "" || a.Token != claim.Token {
		return store.ErrLeaseLost
	}
	p, ok := s.plans[a.PlanID]
	if !ok || p.PublisherID != publisherID {
		return store.ErrNotFound
	}
	now := s.now().UTC()
	a.Status = domain.AttemptFailed
	if result.Status == domain.PlanApplied {
		a.Status = domain.AttemptSucceeded
	}
	res := result
	a.Result = &res
	a.FinishedAt = &now
	s.attempts[attemptKey(publisherID, key)] = a
	if p.Status == domain.PlanPending { // a terminal status is never overwritten
		p.Status = result.Status
		s.plans[p.ID] = p
	}
	s.auditLocked(actor, publisherID, "plan.apply", p.ID, map[string]any{"status": result.Status, "idempotency_key": key})
	s.emitLocked(domain.TopicPlanApplied, map[string]any{"publisher_id": publisherID, "plan_id": p.ID, "status": result.Status})
	return nil
}

// ListAudit implements store.Store, newest first.
func (s *Store) ListAudit(ctx context.Context, publisherID string, limit int) ([]domain.AuditEntry, error) {
	if err := ctx.Err(); err != nil { // honour cancellation like a real database driver
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []domain.AuditEntry{}
	for i := len(s.audit) - 1; i >= 0 && len(out) < limit; i-- {
		if s.audit[i].PublisherID == publisherID {
			out = append(out, s.audit[i])
		}
	}
	return out, nil
}

// ClaimOutbox implements store.Store, oldest first.
func (s *Store) ClaimOutbox(ctx context.Context, limit int, lease time.Duration) ([]domain.Event, error) {
	if err := ctx.Err(); err != nil { // honour cancellation like a real database driver
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := []domain.Event{}
	for _, row := range s.outbox {
		if len(out) >= limit {
			break
		}
		if row.sent || now.Before(row.claimedUntil) {
			continue
		}
		row.claimedUntil = now.Add(lease)
		out = append(out, row.ev)
	}
	return out, nil
}

// MarkOutboxSent implements store.Store.
func (s *Store) MarkOutboxSent(ctx context.Context, ids []string) error {
	if err := ctx.Err(); err != nil { // honour cancellation like a real database driver
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want := make(map[string]bool, len(ids))
	for _, i := range ids {
		want[i] = true
	}
	for _, row := range s.outbox {
		if want[row.ev.ID] {
			row.sent = true
		}
	}
	return nil
}

// CreatePlanForEvent implements store.Store.
func (s *Store) CreatePlanForEvent(ctx context.Context, consumer, eventID string, p domain.Plan) (bool, error) {
	if err := ctx.Err(); err != nil { // honour cancellation like a real database driver
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := consumer + "/" + eventID
	if s.processed[k] {
		return false, nil
	}
	s.processed[k] = true
	s.createPlanLocked(p)
	return true, nil
}

// SetClock replaces the clock; tests use it to move time forward.
func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}
