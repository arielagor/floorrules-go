// Package store defines persistence for rules, plans, idempotent apply
// attempts, the audit log and the transactional outbox. Two implementations
// satisfy the same contract test suite (storetest): an in-memory fake used by
// unit tests, and Postgres via pgx.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/arielagor/floorrules-go/internal/domain"
)

var (
	// ErrNotFound means the entity does not exist (or belongs to another publisher).
	ErrNotFound = errors.New("not found")
	// ErrConflict means an active rule already exists for the segment.
	ErrConflict = errors.New("an active rule already exists for this segment")
	// ErrPlanBusy means a different idempotency key is already applying the plan.
	ErrPlanBusy = errors.New("another apply of this plan is in progress")
	// ErrPlanNotPending means the plan has already been applied (or failed,
	// or gone stale); a new claim on it is refused.
	ErrPlanNotPending = errors.New("plan is not pending; compute a new plan")
	// ErrLeaseLost means the caller's claim on an attempt is no longer
	// current: its lease expired and another worker reclaimed the attempt,
	// or the attempt already finished. The caller's result is not recorded.
	ErrLeaseLost = errors.New("apply lease lost: another worker took over this attempt")
)

// OutboxStats describes the outbox backlog.
type OutboxStats struct {
	// Pending counts live events not yet delivered.
	Pending int
	// OldestPending is the age of the oldest of them, zero when none.
	OldestPending time.Duration
	// Dead counts dead-lettered events waiting for an operator.
	Dead int
}

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

	// GetAttempt returns the publisher's attempt stored under an idempotency
	// key, or ErrNotFound. Keys are scoped to a publisher: the same key under
	// another publisher is a different attempt.
	GetAttempt(ctx context.Context, publisherID, key string) (domain.ApplyAttempt, error)
	// BeginApply claims (a.PublisherID, a.IdempotencyKey) for a.PlanID, which
	// must belong to a.PublisherID (else ErrNotFound). If the key is new it inserts an
	// in_progress attempt and returns (attempt, true). If the key exists it
	// returns the stored attempt and false, except that an in_progress
	// attempt with the same request hash that started more than lease ago is
	// presumed abandoned (its worker crashed or stalled) and is reclaimed:
	// (attempt, true). Lease age is measured on the store's clock, never the
	// caller's. A claimed attempt carries a new Token; any earlier holder's
	// token stops working. A new key is claimed only while the plan is
	// pending, checked under the plan's row lock in the same transaction:
	// otherwise ErrPlanNotPending (ErrNotFound if there is no such plan).
	// Returns ErrPlanBusy if another key holds an in_progress attempt on the
	// plan.
	BeginApply(ctx context.Context, a domain.ApplyAttempt, lease time.Duration) (domain.ApplyAttempt, bool, error)
	// FinishApply records the outcome on the attempt and the plan, and writes
	// an audit entry and a plan.applied event, atomically. It moves the plan
	// only out of pending, never from one terminal status to another. It is fenced: it
	// writes only if a's (publisher, key, Token) is still the current in_progress claim,
	// and otherwise returns ErrLeaseLost and changes nothing. a.Actor is the
	// audit actor. Being fenced, it is safe to retry.
	FinishApply(ctx context.Context, a domain.ApplyAttempt, result domain.ApplyResult) error

	ListAudit(ctx context.Context, publisherID string, limit int) ([]domain.AuditEntry, error)

	// ClaimOutbox leases up to limit unsent, live (not dead-lettered) events,
	// oldest first, so that concurrent relays (one per replica) do not deliver
	// the same rows at the same time. A row's lease is not ownership:
	// delivery is at-least-once and consumers dedupe by event ID.
	ClaimOutbox(ctx context.Context, limit int, lease time.Duration) ([]domain.Event, error)
	// MarkOutboxSent marks events delivered. Callers mark a row only after
	// its delivery succeeded; until then the row is the durable copy. It does
	// not check who holds the claim: if two relays delivered the same row
	// after a lease expired, both marks are correct.
	MarkOutboxSent(ctx context.Context, ids []string) error
	// MarkOutboxFailed records a failed delivery of event id: it counts the
	// attempt, keeps the error, and holds the row back for retryIn before it
	// can be claimed again. When the attempt count reaches maxAttempts the
	// row is dead-lettered instead: it stays in the table for an operator
	// and is never claimed again, and dead is true. A row already sent or
	// dead is left alone.
	MarkOutboxFailed(ctx context.Context, id, reason string, retryIn time.Duration, maxAttempts int) (dead bool, err error)
	// OutboxStats reports the undelivered backlog for monitoring.
	OutboxStats(ctx context.Context) (OutboxStats, error)

	// CreatePlanForEvent records that consumer handled eventID and stores p,
	// atomically. It returns false, storing nothing, if the event was already
	// handled: at-least-once delivery, exactly-once effect.
	CreatePlanForEvent(ctx context.Context, consumer, eventID string, p domain.Plan) (bool, error)
}
