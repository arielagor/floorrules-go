package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ManagedBy is the label this service puts on every floor it writes. The
// planner only ever deletes floors carrying it, so floors that a human set by
// hand in the ad server are never touched.
const ManagedBy = "floorrules"

// PlatformFloor is one floor as it currently exists on the SSP / ad server.
type PlatformFloor struct {
	Segment     Segment `json:"segment"`
	FloorMicros int64   `json:"floor_micros"`
	ManagedBy   string  `json:"managed_by"`
}

// OpKind is the change an operation makes on the platform.
type OpKind string

const (
	OpCreate OpKind = "create"
	OpUpdate OpKind = "update"
	OpDelete OpKind = "delete"
)

// Op is one platform change. Ops are written to be idempotent on the adapter
// (set-to-value and delete-if-present), which is what makes retrying them safe.
type Op struct {
	Kind        OpKind  `json:"kind"`
	Segment     Segment `json:"segment"`
	FromMicros  int64   `json:"from_micros,omitempty"`
	ToMicros    int64   `json:"to_micros,omitempty"`
	Risky       bool    `json:"risky,omitempty"`
	RiskyReason string  `json:"risky_reason,omitempty"`
}

// PlanStatus is the lifecycle state of a plan.
type PlanStatus string

const (
	PlanPending          PlanStatus = "pending"
	PlanApplied          PlanStatus = "applied"
	PlanPartiallyApplied PlanStatus = "partially_applied"
	PlanFailed           PlanStatus = "failed"
	PlanStale            PlanStatus = "stale"
)

// Plan is a reviewed-before-applied diff between desired rules and the
// platform state observed at planning time.
type Plan struct {
	ID              string     `json:"id"`
	PublisherID     string     `json:"publisher_id"`
	Ops             []Op       `json:"ops"`
	BaseFingerprint string     `json:"base_fingerprint"`
	Status          PlanStatus `json:"status"`
	CreatedBy       string     `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
}

// HasRisky reports whether any op needs explicit acknowledgement to apply.
func (p Plan) HasRisky() bool {
	for _, op := range p.Ops {
		if op.Risky {
			return true
		}
	}
	return false
}

// PlanLimits are the blast-radius guardrails applied while planning.
type PlanLimits struct {
	// MaxOps caps how many platform changes one plan may contain.
	MaxOps int
	// RiskyChangePct flags an update whose floor moves by more than this
	// percentage (in either direction) as risky.
	RiskyChangePct int64
}

// DefaultLimits are conservative values for the sample.
var DefaultLimits = PlanLimits{MaxOps: 200, RiskyChangePct: 50}

// ErrPlanTooLarge is returned when a diff exceeds PlanLimits.MaxOps.
var ErrPlanTooLarge = errors.New("plan exceeds the maximum number of operations")

// ComputeOps diffs active rules against platform floors. Output order is
// deterministic (deletes, then updates, then creates; each sorted by segment
// key) so the same inputs always produce the same plan.
func ComputeOps(rules []Rule, platform []PlatformFloor, limits PlanLimits) ([]Op, error) {
	desired := make(map[string]Rule, len(rules))
	for _, r := range rules {
		if r.Status != RuleActive {
			continue
		}
		desired[r.Segment.Key()] = r
	}
	current := make(map[string]PlatformFloor, len(platform))
	for _, f := range platform {
		current[f.Segment.Key()] = f
	}

	var deletes, updates, creates []Op
	for key, f := range current {
		if _, want := desired[key]; !want && f.ManagedBy == ManagedBy {
			deletes = append(deletes, Op{Kind: OpDelete, Segment: f.Segment, FromMicros: f.FloorMicros})
		}
	}
	for key, r := range desired {
		f, exists := current[key]
		switch {
		case !exists:
			creates = append(creates, Op{Kind: OpCreate, Segment: r.Segment, ToMicros: r.FloorMicros})
		case f.FloorMicros != r.FloorMicros || f.ManagedBy != ManagedBy:
			op := Op{Kind: OpUpdate, Segment: r.Segment, FromMicros: f.FloorMicros, ToMicros: r.FloorMicros}
			if f.ManagedBy != ManagedBy {
				op.Risky = true
				op.RiskyReason = "takes over a floor not managed by this service"
			} else if pct := changePct(f.FloorMicros, r.FloorMicros); pct > limits.RiskyChangePct {
				op.Risky = true
				op.RiskyReason = fmt.Sprintf("floor changes by %d%%, over the %d%% guardrail", pct, limits.RiskyChangePct)
			}
			updates = append(updates, op)
		}
	}

	for _, ops := range [][]Op{deletes, updates, creates} {
		sort.Slice(ops, func(i, j int) bool { return ops[i].Segment.Key() < ops[j].Segment.Key() })
	}
	out := make([]Op, 0, len(deletes)+len(updates)+len(creates))
	out = append(out, deletes...)
	out = append(out, updates...)
	out = append(out, creates...)

	if limits.MaxOps > 0 && len(out) > limits.MaxOps {
		return nil, fmt.Errorf("%w: %d ops, limit %d", ErrPlanTooLarge, len(out), limits.MaxOps)
	}
	return out, nil
}

// changePct returns the absolute percentage change from a to b, rounded down.
// a is always > 0 for a platform floor; a zero base is treated as 100%.
func changePct(from, to int64) int64 {
	if from <= 0 {
		return 100
	}
	d := to - from
	if d < 0 {
		d = -d
	}
	return d * 100 / from
}

// Fingerprint hashes platform state in an order-independent way. A plan stores
// the fingerprint it was computed against; apply refuses to run if the
// platform has drifted since, because the diff would no longer be correct.
func Fingerprint(platform []PlatformFloor) string {
	lines := make([]string, 0, len(platform))
	for _, f := range platform {
		lines = append(lines, fmt.Sprintf("%s=%d@%s", f.Segment.Key(), f.FloorMicros, f.ManagedBy))
	}
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}
