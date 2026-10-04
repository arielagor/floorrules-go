// Package adapter is the seam between the service and an SSP / ad server.
// The real integration (an SSP's REST API) would implement SSP; the sample
// ships an in-memory Mock with fault injection.
package adapter

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/arielagor/floorrules/internal/domain"
)

// SSP is the minimal platform contract the service needs. SetFloor and
// DeleteFloor must be idempotent: setting the same value twice, or deleting a
// floor that is already gone, succeeds. Apply relies on that to retry safely.
type SSP interface {
	ListFloors(ctx context.Context, publisherID string) ([]domain.PlatformFloor, error)
	SetFloor(ctx context.Context, publisherID string, f domain.PlatformFloor) error
	DeleteFloor(ctx context.Context, publisherID string, s domain.Segment) error
}

// StatusError models a non-2xx response from the platform API.
type StatusError struct {
	Status     int
	RetryAfter time.Duration // zero when the platform sent no Retry-After
	Msg        string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("ssp responded %d: %s", e.Status, e.Msg)
}

// Retryable classifies an error. Only throttling, server errors and transport
// timeouts are retried; a 4xx means the request itself is wrong and retrying
// it would just repeat the failure.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	// A per-attempt deadline is retryable; the caller's overall deadline is
	// checked separately by the retry loop before sleeping.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status == http.StatusTooManyRequests || se.Status >= 500
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// retryAfter extracts a server-requested delay, if any.
func retryAfter(err error) time.Duration {
	var se *StatusError
	if errors.As(err, &se) {
		return se.RetryAfter
	}
	return 0
}
