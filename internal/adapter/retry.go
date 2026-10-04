package adapter

import (
	"context"
	"math/rand/v2"
	"time"
)

// RetryPolicy is exponential backoff with full jitter, a per-attempt timeout,
// and a cap on how long a server's Retry-After is honoured.
type RetryPolicy struct {
	MaxAttempts   int
	BaseDelay     time.Duration
	MaxDelay      time.Duration
	PerAttempt    time.Duration
	MaxRetryAfter time.Duration

	// Test seams. Nil means real time and real randomness.
	Sleep  func(ctx context.Context, d time.Duration) error
	Jitter func(n int64) int64
}

// DefaultRetryPolicy suits a platform API with second-scale latency.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:   5,
		BaseDelay:     100 * time.Millisecond,
		MaxDelay:      5 * time.Second,
		PerAttempt:    10 * time.Second,
		MaxRetryAfter: 30 * time.Second,
	}
}

// Delay returns the backoff before retry number attempt (1-based): a uniform
// random value in [0, min(MaxDelay, BaseDelay*2^(attempt-1))]. Full jitter
// spreads retries from many workers so they don't hit a recovering platform
// in lockstep.
func (p RetryPolicy) Delay(attempt int) time.Duration {
	ceiling := p.BaseDelay
	for i := 1; i < attempt && ceiling < p.MaxDelay; i++ {
		ceiling *= 2
	}
	ceiling = min(ceiling, p.MaxDelay)
	if ceiling <= 0 {
		return 0
	}
	jitter := p.Jitter
	if jitter == nil {
		jitter = rand.Int64N
	}
	return time.Duration(jitter(int64(ceiling) + 1))
}

// Do runs fn until it succeeds, returns a non-retryable error, runs out of
// attempts, or ctx ends. It returns the number of attempts made and the last
// error.
func (p RetryPolicy) Do(ctx context.Context, fn func(ctx context.Context) error) (int, error) {
	sleep := p.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	maxAttempts := max(p.MaxAttempts, 1)

	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = p.once(ctx, fn)
		if err == nil {
			return attempt, nil
		}
		if !Retryable(err) || attempt == maxAttempts {
			return attempt, err
		}
		if ctx.Err() != nil {
			return attempt, ctx.Err()
		}
		wait := p.Delay(attempt)
		if ra := retryAfter(err); ra > 0 {
			wait = max(wait, min(ra, p.MaxRetryAfter))
		}
		// Don't start a sleep that will outlive the caller's deadline.
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < wait {
			return attempt, err
		}
		if serr := sleep(ctx, wait); serr != nil {
			return attempt, serr
		}
	}
	return maxAttempts, err
}

func (p RetryPolicy) once(ctx context.Context, fn func(ctx context.Context) error) error {
	if p.PerAttempt <= 0 {
		return fn(ctx)
	}
	actx, cancel := context.WithTimeout(ctx, p.PerAttempt)
	defer cancel()
	return fn(actx)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
