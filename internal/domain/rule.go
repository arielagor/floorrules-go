// Package domain holds the floor-rule model, its validation, and the pure
// planning logic. Nothing in here does I/O, so all of it is unit-testable.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Any is the wildcard value for a segment dimension.
const Any = "*"

// Money is held as integer micros of the currency unit (1 USD CPM = 1_000_000)
// so floor arithmetic never touches floating point.
const (
	MicrosPerUnit   int64 = 1_000_000
	MinFloorMicros  int64 = 10_000      // $0.01 CPM
	MaxFloorMicros  int64 = 500_000_000 // $500.00 CPM, a sanity ceiling
	DefaultCurrency       = "USD"
)

// RuleStatus is the lifecycle state of a rule.
type RuleStatus string

// Rule statuses.
const (
	RuleActive   RuleStatus = "active"
	RuleDisabled RuleStatus = "disabled"
)

// Segment is the targeting key a floor applies to. Every dimension is either
// a concrete value or Any.
type Segment struct {
	Device        string `json:"device"`
	Geo           string `json:"geo"`
	Genre         string `json:"genre"`
	DemandPartner string `json:"demand_partner"`
}

// Key is a stable, human-readable identity for the segment, used to diff
// desired state against platform state.
func (s Segment) Key() string {
	return strings.Join([]string{s.Device, s.Geo, s.Genre, s.DemandPartner}, "|")
}

// Rule is an operator-defined floor for one publisher segment.
type Rule struct {
	ID          string     `json:"id"`
	PublisherID string     `json:"publisher_id"`
	Segment     Segment    `json:"segment"`
	FloorMicros int64      `json:"floor_micros"`
	Currency    string     `json:"currency"`
	Status      RuleStatus `json:"status"`
	Version     int        `json:"version"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// ValidationError lists every field problem at once so an operator can fix a
// rule in one round trip instead of one error at a time.
type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for k, v := range e.Fields {
		parts = append(parts, k+": "+v)
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// ErrValidation lets callers use errors.Is without caring about the details.
var ErrValidation = errors.New("validation failed")

// Is makes errors.Is(err, ErrValidation) true for any *ValidationError.
func (e *ValidationError) Is(target error) bool { return target == ErrValidation }

var (
	publisherRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	slugRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	geoRe       = regexp.MustCompile(`^[A-Z]{2}$`)
	validDevice = map[string]bool{"ctv": true, "mobile": true, "desktop": true, "tablet": true}
)

// ValidPublisherID reports whether id is an acceptable publisher identifier.
// It is also used to validate path parameters before they reach storage.
func ValidPublisherID(id string) bool { return publisherRe.MatchString(id) }

// NormalizeAndValidate canonicalises a rule in place (blank dimensions become
// Any, geo is upper-cased, currency defaults to USD) and validates it.
func NormalizeAndValidate(r *Rule) error {
	fields := map[string]string{}

	r.PublisherID = strings.TrimSpace(r.PublisherID)
	if !ValidPublisherID(r.PublisherID) {
		fields["publisher_id"] = "must match ^[a-z0-9][a-z0-9-]{0,62}$"
	}

	s := &r.Segment
	s.Device = norm(strings.ToLower(s.Device))
	s.Geo = norm(strings.ToUpper(s.Geo))
	s.Genre = norm(strings.ToLower(s.Genre))
	s.DemandPartner = norm(strings.ToLower(s.DemandPartner))

	if s.Device != Any && !validDevice[s.Device] {
		fields["segment.device"] = "must be one of ctv, mobile, desktop, tablet or *"
	}
	if s.Geo != Any && !geoRe.MatchString(s.Geo) {
		fields["segment.geo"] = "must be an ISO 3166-1 alpha-2 code or *"
	}
	if s.Genre != Any && !slugRe.MatchString(s.Genre) {
		fields["segment.genre"] = "must be a lowercase slug or *"
	}
	if s.DemandPartner != Any && !slugRe.MatchString(s.DemandPartner) {
		fields["segment.demand_partner"] = "must be a lowercase slug or *"
	}

	if r.FloorMicros < MinFloorMicros || r.FloorMicros > MaxFloorMicros {
		fields["floor_micros"] = fmt.Sprintf("must be between %d and %d", MinFloorMicros, MaxFloorMicros)
	}

	if r.Currency == "" {
		r.Currency = DefaultCurrency
	}
	if r.Currency != DefaultCurrency {
		fields["currency"] = "only USD is supported"
	}

	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}

func norm(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return Any
	}
	return v
}
