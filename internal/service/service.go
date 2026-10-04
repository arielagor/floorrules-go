// Package service orchestrates rules, planning and safe apply. It owns the
// business rules; the HTTP layer only translates requests and errors.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/arielagor/floorrules-go/internal/adapter"
	"github.com/arielagor/floorrules-go/internal/domain"
	"github.com/arielagor/floorrules-go/internal/events"
	"github.com/arielagor/floorrules-go/internal/id"
	"github.com/arielagor/floorrules-go/internal/metrics"
	"github.com/arielagor/floorrules-go/internal/store"
)

// Errors the API maps to HTTP statuses.
var (
	ErrNotFound              = store.ErrNotFound
	ErrConflict              = store.ErrConflict
	ErrPlanNotPending        = store.ErrPlanNotPending
	ErrRiskyNotAcknowledged  = errors.New("plan contains risky ops; resend with acknowledge_risky=true after review")
	ErrApplyInProgress       = errors.New("an apply for this plan or key is already in progress")
	ErrIdempotencyKeyReused  = errors.New("idempotency key was already used for a different request")
	ErrInvalidIdempotencyKey = errors.New("Idempotency-Key header must be 8-128 chars of [A-Za-z0-9._:-]")
	ErrPlatformUnavailable   = errors.New("ad platform unavailable")
	ErrShuttingDown          = errors.New("instance is shutting down; retry the request")
	// ErrLeaseLost: this worker stalled past its lease and another took the
	// apply over. Its result was discarded; the successor's stands.
	ErrLeaseLost = store.ErrLeaseLost
)

// AutoPlannerConsumer names the rule.changed consumer for dedupe records.
const AutoPlannerConsumer = "auto-planner"

// Config tunes the service.
type Config struct {
	Limits       domain.PlanLimits
	Retry        adapter.RetryPolicy
	ApplyLease   time.Duration // after this, an in_progress attempt is presumed abandoned
	ApplyTimeout time.Duration // overall budget for executing one plan
	// RecordTimeout is the separate budget for writing an apply's result.
	// It starts after execution, so a slow platform that uses the whole
	// ApplyTimeout cannot also prevent the result from being recorded.
	RecordTimeout time.Duration
	Now           func() time.Time
	Log           *slog.Logger
	Metrics       *metrics.Registry
}

// Service is safe for concurrent use.
type Service struct {
	store store.Store
	ssp   adapter.SSP
	cfg   Config

	// Shutdown bookkeeping. Once draining is set no new apply starts, so
	// inflight.Add never races inflight.Wait.
	mu       sync.Mutex
	draining bool
	inflight sync.WaitGroup
}

// New builds a Service, filling unset config with defaults. It refuses a
// lease that an apply can outlive: a second worker would reclaim an attempt
// that is still running, and both would write to the platform.
func New(st store.Store, ssp adapter.SSP, cfg Config) (*Service, error) {
	if cfg.Limits.MaxOps == 0 {
		cfg.Limits = domain.DefaultLimits
	}
	if cfg.Retry.MaxAttempts == 0 {
		cfg.Retry = adapter.DefaultRetryPolicy()
	}
	if cfg.ApplyLease == 0 {
		cfg.ApplyLease = 5 * time.Minute
	}
	if cfg.ApplyTimeout == 0 {
		cfg.ApplyTimeout = 2 * time.Minute
	}
	if cfg.RecordTimeout == 0 {
		cfg.RecordTimeout = 10 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.ApplyLease <= cfg.ApplyTimeout+cfg.RecordTimeout {
		return nil, fmt.Errorf("service: ApplyLease (%s) must exceed ApplyTimeout + RecordTimeout (%s)",
			cfg.ApplyLease, cfg.ApplyTimeout+cfg.RecordTimeout)
	}
	return &Service{store: st, ssp: ssp, cfg: cfg}, nil
}

// Ready reports whether dependencies are reachable.
func (s *Service) Ready(ctx context.Context) error { return s.store.Ping(ctx) }

// MaxApplyDuration is the longest one apply can take from claim to recorded
// result: the execution budget plus the separate recording budget. Shutdown
// must allow at least this long, or a deploy can cut an apply off midway.
func (s *Service) MaxApplyDuration() time.Duration {
	return s.cfg.ApplyTimeout + s.cfg.RecordTimeout
}

// WaitForApplies stops new applies (they get ErrShuttingDown) and blocks
// until every apply already started has recorded its result, or ctx ends.
func (s *Service) WaitForApplies(ctx context.Context) error {
	s.mu.Lock()
	s.draining = true
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// enterApply registers an apply with the shutdown tracker. It reports false
// once shutdown has begun.
func (s *Service) enterApply() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return false
	}
	s.inflight.Add(1)
	return true
}

// CreateRule validates and stores a rule.
func (s *Service) CreateRule(ctx context.Context, actor string, r domain.Rule) (domain.Rule, error) {
	if err := domain.NormalizeAndValidate(&r); err != nil {
		return domain.Rule{}, err
	}
	r.ID = id.New()
	r.CreatedBy = actor
	return s.store.CreateRule(ctx, r)
}

// DisableRule soft-deletes a rule.
func (s *Service) DisableRule(ctx context.Context, actor, publisherID, ruleID string) (domain.Rule, error) {
	return s.store.DisableRule(ctx, publisherID, ruleID, actor)
}

// ListRules returns every rule (active and disabled) for a publisher.
func (s *Service) ListRules(ctx context.Context, publisherID string) ([]domain.Rule, error) {
	return s.store.ListRules(ctx, publisherID)
}

// ListAudit returns the newest audit entries for a publisher.
func (s *Service) ListAudit(ctx context.Context, publisherID string, limit int) ([]domain.AuditEntry, error) {
	return s.store.ListAudit(ctx, publisherID, limit)
}

// GetPlan returns one plan.
func (s *Service) GetPlan(ctx context.Context, publisherID, planID string) (domain.Plan, error) {
	return s.store.GetPlan(ctx, publisherID, planID)
}

func (s *Service) listFloors(ctx context.Context, publisherID string) ([]domain.PlatformFloor, error) {
	var floors []domain.PlatformFloor
	attempts, err := s.cfg.Retry.Do(ctx, func(ctx context.Context) error {
		var err error
		floors, err = s.ssp.ListFloors(ctx, publisherID)
		return err
	})
	s.recordAdapterCall("list", attempts, err)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPlatformUnavailable, err)
	}
	return floors, nil
}

// buildPlan reads current state and diffs it. It does not persist.
func (s *Service) buildPlan(ctx context.Context, actor, publisherID string) (domain.Plan, error) {
	rules, err := s.store.ListRules(ctx, publisherID)
	if err != nil {
		return domain.Plan{}, err
	}
	floors, err := s.listFloors(ctx, publisherID)
	if err != nil {
		return domain.Plan{}, err
	}
	ops, err := domain.ComputeOps(rules, floors, s.cfg.Limits)
	if err != nil {
		return domain.Plan{}, err
	}
	return domain.Plan{
		ID: id.New(), PublisherID: publisherID, Ops: ops, BaseFingerprint: domain.Fingerprint(floors),
		Status: domain.PlanPending, CreatedBy: actor, CreatedAt: s.cfg.Now().UTC(),
	}, nil
}

// CreatePlan computes and stores a plan for review. Nothing changes on the
// platform until the plan is applied.
func (s *Service) CreatePlan(ctx context.Context, actor, publisherID string) (domain.Plan, error) {
	p, err := s.buildPlan(ctx, actor, publisherID)
	if err != nil {
		return domain.Plan{}, err
	}
	if err := s.store.CreatePlan(ctx, p); err != nil {
		return domain.Plan{}, err
	}
	return p, nil
}

var idemKeyRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

func requestHash(planID string, ackRisky bool) string {
	sum := sha256.Sum256([]byte(planID + "|" + strconv.FormatBool(ackRisky)))
	return hex.EncodeToString(sum[:])
}

// ApplyPlan executes a plan exactly once per idempotency key. A repeat of a
// finished request returns the stored result with replayed=true and makes no
// platform calls.
func (s *Service) ApplyPlan(ctx context.Context, actor, publisherID, planID, key string, ackRisky bool) (domain.ApplyResult, bool, error) {
	if !idemKeyRe.MatchString(key) {
		return domain.ApplyResult{}, false, ErrInvalidIdempotencyKey
	}
	if !s.enterApply() {
		return domain.ApplyResult{}, false, ErrShuttingDown
	}
	defer s.inflight.Done()
	hash := requestHash(planID, ackRisky)

	// 1. A key we have seen is answered from the record, before any state
	// checks. An in-progress attempt for the same request goes on to
	// BeginApply, which decides on the database's clock whether its lease
	// has expired and it may be reclaimed.
	if prior, err := s.store.GetAttempt(ctx, key); err == nil {
		if prior.Status != domain.AttemptInProgress || prior.RequestHash != hash {
			return s.replay(prior, hash)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return domain.ApplyResult{}, false, err
	}

	// 2. Preconditions. Failing these does not consume the key.
	plan, err := s.store.GetPlan(ctx, publisherID, planID)
	if err != nil {
		return domain.ApplyResult{}, false, err
	}
	if plan.Status != domain.PlanPending {
		return domain.ApplyResult{}, false, ErrPlanNotPending
	}
	if plan.HasRisky() && !ackRisky {
		return domain.ApplyResult{}, false, ErrRiskyNotAcknowledged
	}

	// 3. Claim the key. Losing a race to the same key falls back to replay.
	attempt, created, err := s.store.BeginApply(ctx, domain.ApplyAttempt{
		IdempotencyKey: key, PlanID: planID, RequestHash: hash, Actor: actor,
	}, s.cfg.ApplyLease)
	if errors.Is(err, store.ErrPlanBusy) {
		return domain.ApplyResult{}, false, ErrApplyInProgress
	}
	if err != nil {
		return domain.ApplyResult{}, false, err
	}
	if !created {
		return s.replay(attempt, hash)
	}

	// 4. Execute, detached from the caller: a client disconnect must not
	// abandon a plan halfway. The apply has its own deadline instead.
	execCtx, cancelExec := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ApplyTimeout)
	result := s.execute(execCtx, plan)
	cancelExec()

	// 5. Record on a fresh budget. execCtx may be exhausted by now (a slow
	// platform uses all of it), and writing the result on it would lose the
	// audit row and event for changes that did happen.
	recCtx, cancelRec := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.RecordTimeout)
	defer cancelRec()
	if err := s.record(recCtx, publisherID, attempt, result); err != nil {
		if errors.Is(err, store.ErrLeaseLost) {
			// Another worker reclaimed this attempt while we were stalled.
			// Its record stands; ours is discarded rather than overwriting it.
			s.cfg.Log.Warn("apply result discarded: lease was taken over", "plan_id", planID, "key", key,
				"status", result.Status)
			s.cfg.Metrics.Inc("apply_lease_lost_total")
			return domain.ApplyResult{}, false, ErrLeaseLost
		}
		s.cfg.Log.Error("apply finished but not recorded", "plan_id", planID, "key", key, "err", err)
		s.cfg.Metrics.Inc("apply_record_failures_total")
		return domain.ApplyResult{}, false, err
	}
	s.cfg.Metrics.Inc("apply_total", "status", string(result.Status))
	s.cfg.Log.Info("plan applied", "plan_id", planID, "publisher_id", publisherID, "status", result.Status, "actor", actor)
	return result, false, nil
}

// record writes an apply's result, retrying transient store errors within
// ctx. Retrying is safe because FinishApply is fenced on the attempt token:
// a retry after a commit whose reply was lost gets ErrLeaseLost, which is
// told apart from a real takeover by re-reading the attempt.
func (s *Service) record(ctx context.Context, publisherID string, a domain.ApplyAttempt, result domain.ApplyResult) error {
	var err error
	for try := 1; ; try++ {
		err = s.store.FinishApply(ctx, publisherID, a, result)
		if err == nil || errors.Is(err, store.ErrNotFound) {
			return err
		}
		if errors.Is(err, store.ErrLeaseLost) {
			if try > 1 && s.recordedByUs(ctx, a) {
				return nil
			}
			return err
		}
		if try == 3 || ctx.Err() != nil {
			return err
		}
		s.cfg.Log.Warn("recording apply result failed; retrying", "key", a.IdempotencyKey, "try", try, "err", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(try) * 200 * time.Millisecond):
		}
	}
}

// recordedByUs reports whether the attempt finished under our token, i.e. an
// earlier FinishApply committed even though its reply was lost.
func (s *Service) recordedByUs(ctx context.Context, a domain.ApplyAttempt) bool {
	got, err := s.store.GetAttempt(ctx, a.IdempotencyKey)
	return err == nil && got.Status != domain.AttemptInProgress && got.Token == a.Token
}

func (s *Service) replay(a domain.ApplyAttempt, hash string) (domain.ApplyResult, bool, error) {
	if a.RequestHash != hash {
		return domain.ApplyResult{}, false, ErrIdempotencyKeyReused
	}
	if a.Status == domain.AttemptInProgress || a.Result == nil {
		return domain.ApplyResult{}, false, ErrApplyInProgress
	}
	return *a.Result, true, nil
}

// execute performs a plan's ops in order, stopping at the first permanent
// failure. It never returns an error: every outcome is a result to record.
func (s *Service) execute(ctx context.Context, plan domain.Plan) domain.ApplyResult {
	res := domain.ApplyResult{PlanID: plan.ID, Results: make([]domain.OpResult, 0, len(plan.Ops))}

	floors, err := s.listFloors(ctx, plan.PublisherID)
	if err != nil {
		res.Status, res.Reason = domain.PlanFailed, "could not read platform state: "+err.Error()
		return res
	}
	if domain.Fingerprint(floors) != plan.BaseFingerprint {
		res.Status, res.Reason = domain.PlanStale, "platform state changed since the plan was computed; compute a new plan"
		return res
	}

	applied, failed := 0, false
	for _, op := range plan.Ops {
		if failed {
			res.Results = append(res.Results, domain.OpResult{Op: op, Outcome: domain.OutcomeSkipped})
			continue
		}
		attempts, err := s.cfg.Retry.Do(ctx, func(ctx context.Context) error { return s.applyOp(ctx, plan.PublisherID, op) })
		s.recordAdapterCall(string(op.Kind), attempts, err)
		r := domain.OpResult{Op: op, Attempts: attempts, Outcome: domain.OutcomeApplied}
		if err != nil {
			r.Outcome, r.Error, failed = domain.OutcomeFailed, err.Error(), true
		} else {
			applied++
		}
		res.Results = append(res.Results, r)
	}

	switch {
	case !failed:
		res.Status = domain.PlanApplied
	case applied > 0:
		res.Status, res.Reason = domain.PlanPartiallyApplied, "an op failed after earlier ops were applied; compute a new plan to converge"
	default:
		res.Status, res.Reason = domain.PlanFailed, "the first op failed; nothing was changed"
	}
	return res
}

func (s *Service) applyOp(ctx context.Context, publisherID string, op domain.Op) error {
	switch op.Kind {
	case domain.OpCreate, domain.OpUpdate:
		return s.ssp.SetFloor(ctx, publisherID, domain.PlatformFloor{
			Segment: op.Segment, FloorMicros: op.ToMicros, ManagedBy: domain.ManagedBy,
		})
	case domain.OpDelete:
		return s.ssp.DeleteFloor(ctx, publisherID, op.Segment)
	default:
		return fmt.Errorf("unknown op kind %q", op.Kind)
	}
}

func (s *Service) recordAdapterCall(op string, attempts int, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	s.cfg.Metrics.Inc("ssp_calls_total", "op", op, "outcome", outcome)
	if attempts > 1 {
		s.cfg.Metrics.Add("ssp_retries_total", float64(attempts-1), "op", op)
	}
}

// HandleRuleChanged is the rule.changed consumer: it proposes a plan for the
// publisher so an operator can review and apply it. Duplicate deliveries of
// the same event are absorbed by the store's processed-events record.
func (s *Service) HandleRuleChanged(ctx context.Context, m events.Message) error {
	var p domain.RuleChangedPayload
	if err := json.Unmarshal(m.Payload, &p); err != nil || !domain.ValidPublisherID(p.PublisherID) {
		// A malformed event will never succeed; log it and ack rather than
		// redelivering it forever.
		s.cfg.Log.Error("dropping malformed rule.changed event", "event_id", m.ID, "err", err)
		return nil
	}
	plan, err := s.buildPlan(ctx, "system:"+AutoPlannerConsumer, p.PublisherID)
	if err != nil {
		return err
	}
	if len(plan.Ops) == 0 {
		// Platform already matches (e.g. the change was applied before this
		// event arrived). Nothing to review, so propose nothing. No dedupe row
		// is needed: this path has no side effects, so redelivery is harmless.
		s.cfg.Metrics.Inc("auto_plans_skipped_total", "reason", "no_drift")
		s.cfg.Log.Info("rule change needs no plan: platform already matches", "event_id", m.ID, "publisher_id", p.PublisherID)
		return nil
	}
	created, err := s.store.CreatePlanForEvent(ctx, AutoPlannerConsumer, m.ID, plan)
	if err != nil {
		return err
	}
	if created {
		s.cfg.Metrics.Inc("auto_plans_total")
		s.cfg.Log.Info("proposed plan from rule change", "event_id", m.ID, "plan_id", plan.ID, "ops", len(plan.Ops))
	} else {
		s.cfg.Metrics.Inc("events_deduplicated_total", "consumer", AutoPlannerConsumer)
	}
	return nil
}
