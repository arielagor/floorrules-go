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
)

// Event is an outbox row and, once relayed, a queue message.
type Event struct {
	ID        string          `json:"id"`
	Topic     string          `json:"topic"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// RuleChangedPayload is the body of a rule.changed event.
type RuleChangedPayload struct {
	PublisherID string `json:"publisher_id"`
	RuleID      string `json:"rule_id"`
	Change      string `json:"change"` // created | disabled
}
