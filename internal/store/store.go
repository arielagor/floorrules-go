// Package store defines persistence for rules, plans, idempotent apply
// attempts, the audit log and the transactional outbox. Two implementations
// satisfy the same contract test suite (storetest): an in-memory fake used by
// unit tests, and Postgres via pgx.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/arielagor/floorrules/internal/domain"
)

var (
	// ErrNotFound means the entity does not exist (or belongs to another publisher).
	ErrNotFound = errors.New("not found")
	// ErrConflict means an active rule already exists for the segment.
	ErrConflict = errors.New("an active rule already exists for this segment")
	// ErrPlanBusy means a different idempotency key is already applying the plan.
	ErrPlanBusy = errors.New("another apply of this plan is in progress")
)

// Store is the persistence contract. Every method that changes state writes
// its audit entry and, where relevant, its outbox event in the same
// transaction as the change itself, so an event is never published for a
// change that rolled back and a change never commits without its event.
type Store interface {
	Ping(ctx context.Context) error

	// CreateRule inserts an active rule, an audit entry and a rule.changed event.
	CreateRule(ctx context.Context, r domain.Rule) (domain.Rule, error)
	// DisableRule soft-deletes a rule, with audit entry and rule.changed event.
	DisableRule(ctx context.Context, publisherID, ruleID, actor string) (domain.Rule, error)
	ListRules(ctx context.Context, publisherID string) ([]domain.Rule, error)

	// CreatePlan stores a pending plan and its audit entry.
	CreatePlan(ctx context.Context, p domain.Plan) error
	GetPlan(ctx context.Context, publisherID, planID string) (domain.Plan, error)

	// BeginApply claims an idempotency key. If the key is new it inserts a
	// in_progress attempt and returns (attempt, true). If the key exists it
	// returns the stored attempt and false, except that an in_progress
	// attempt with the same request hash that started before leaseCutoff is
	// presumed abandoned (its worker crashed) and is reclaimed: (attempt, true).
	// Returns ErrPlanBusy if another key holds an in_progress attempt on the plan.
	BeginApply(ctx context.Context, a domain.ApplyAttempt, leaseCutoff time.Time) (domain.ApplyAttempt, bool, error)
	// FinishApply records the outcome on the attempt and the plan, and writes
	// an audit entry and a plan.applied event, atomically.
	FinishApply(ctx context.Context, publisherID, key, actor string, result domain.ApplyResult) error

	ListAudit(ctx context.Context, publisherID string, limit int) ([]domain.AuditEntry, error)

	// ClaimOutbox leases up to limit unsent events so that concurrent relays
	// (one per replica) do not publish the same rows at the same time.
	ClaimOutbox(ctx context.Context, limit int, lease time.Duration) ([]domain.Event, error)
	MarkOutboxSent(ctx context.Context, ids []string) error

	// CreatePlanForEvent records that consumer handled eventID and stores p,
	// atomically. It returns false, storing nothing, if the event was already
	// handled: at-least-once delivery, exactly-once effect.
	CreatePlanForEvent(ctx context.Context, consumer, eventID string, p domain.Plan) (bool, error)
}
