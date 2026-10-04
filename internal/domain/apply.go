package domain

import (
	"encoding/json"
	"time"
)

// OpOutcome records what happened to one op during apply.
type OpOutcome string

// Op outcomes.
const (
	OutcomeApplied OpOutcome = "applied"
	OutcomeFailed  OpOutcome = "failed"
	OutcomeSkipped OpOutcome = "skipped" // not attempted because an earlier op failed
)

// OpResult is the per-op record kept with an apply attempt.
type OpResult struct {
	Op       Op        `json:"op"`
	Outcome  OpOutcome `json:"outcome"`
	Attempts int       `json:"attempts"`
	Error    string    `json:"error,omitempty"`
}

// ApplyResult is what an apply returns, and what a replay of the same
// idempotency key returns again, byte for byte.
type ApplyResult struct {
	PlanID  string     `json:"plan_id"`
	Status  PlanStatus `json:"status"`
	Reason  string     `json:"reason,omitempty"`
	Results []OpResult `json:"results"`
}

// AttemptStatus is the state of one idempotent apply attempt.
type AttemptStatus string

// Attempt statuses.
const (
	AttemptInProgress AttemptStatus = "in_progress"
	AttemptSucceeded  AttemptStatus = "succeeded"
	AttemptFailed     AttemptStatus = "failed"
	// AttemptAbandoned marks an in_progress attempt whose lease expired and
	// which another key superseded: its worker died or stalled, possibly
	// after writing to the platform. It is terminal and never replayed.
	AttemptAbandoned AttemptStatus = "abandoned"
)

// ApplyAttempt is keyed by the client's Idempotency-Key.
type ApplyAttempt struct {
	PublisherID    string        `json:"publisher_id"` // keys are scoped to a publisher
	IdempotencyKey string        `json:"idempotency_key"`
	PlanID         string        `json:"plan_id"`
	RequestHash    string        `json:"request_hash"`
	Status         AttemptStatus `json:"status"`
	Actor          string        `json:"actor"`
	Result         *ApplyResult  `json:"result,omitempty"`
	StartedAt      time.Time     `json:"started_at"`
	FinishedAt     *time.Time    `json:"finished_at,omitempty"`
	// Token fences the attempt: it changes on every claim (insert or lease
	// reclaim), and only the holder of the current token may record a result.
	Token string `json:"-"`
	// FollowsInterrupted is set on a claim that took over from an earlier
	// claim on the same plan whose lease expired (a reclaim of this key, or
	// a new key superseding an abandoned one). The platform may already
	// hold some of the plan's writes. Not stored.
	FollowsInterrupted bool `json:"-"`
}

// AuditEntry is an append-only record of who changed what.
type AuditEntry struct {
	ID          int64           `json:"id"`
	At          time.Time       `json:"at"`
	Actor       string          `json:"actor"`
	PublisherID string          `json:"publisher_id"`
	Action      string          `json:"action"`
	EntityID    string          `json:"entity_id"`
	Detail      json.RawMessage `json:"detail,omitempty"`
}

// Event topics written to the transactional outbox.
const (
	TopicRuleChanged = "rule.changed"
	TopicPlanApplied = "plan.applied"
	// TopicPlanApplyAbandoned announces an attempt superseded after its
	// lease expired, for whoever reconciles interrupted applies.
	TopicPlanApplyAbandoned = "plan.apply_abandoned"
)

// Event is an outbox row and, once relayed, a queue message.
type Event struct {
	ID        string          `json:"id"`
	Topic     string          `json:"topic"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
	// Attempts counts earlier deliveries of this event that failed.
	Attempts int `json:"attempts"`
}

// RuleChangedPayload is the body of a rule.changed event.
type RuleChangedPayload struct {
	PublisherID string `json:"publisher_id"`
	RuleID      string `json:"rule_id"`
	Change      string `json:"change"` // created | disabled
}
