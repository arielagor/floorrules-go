package adapter

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/arielagor/floorrules-go/internal/domain"
)

// fakeTimeout satisfies net.Error with Timeout() == true.
type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "i/o timeout" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return true }

func TestRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&StatusError{Status: 429}, true},
		{&StatusError{Status: 500}, true},
		{&StatusError{Status: 503}, true},
		{&StatusError{Status: 400}, false},
		{&StatusError{Status: 404}, false},
		{&StatusError{Status: 409}, false},
		{fmt.Errorf("wrapped: %w", &StatusError{Status: 502}), true},
		{context.Canceled, false},
		{context.DeadlineExceeded, true},
		{fakeTimeout{}, true},
		{errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := Retryable(c.err); got != c.want {
			t.Errorf("Retryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// testPolicy records sleeps instead of sleeping and uses max jitter so the
// exponential ceiling is observable.
func testPolicy(slept *[]time.Duration) RetryPolicy {
	return RetryPolicy{
		MaxAttempts:   5,
		BaseDelay:     100 * time.Millisecond,
		MaxDelay:      1 * time.Second,
		MaxRetryAfter: 3 * time.Second,
		Sleep: func(_ context.Context, d time.Duration) error {
			*slept = append(*slept, d)
			return nil
		},
		Jitter: func(n int64) int64 { return n - 1 },
	}
}

func TestDo_RetriesTransientThenSucceeds(t *testing.T) {
	var slept []time.Duration
	p := testPolicy(&slept)
	calls := 0
	attempts, err := p.Do(context.Background(), func(context.Context) error {
		calls++
		if calls < 4 {
			return &StatusError{Status: 503}
		}
		return nil
	})
	if err != nil || attempts != 4 {
		t.Fatalf("attempts=%d err=%v, want 4, nil", attempts, err)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	if fmt.Sprint(slept) != fmt.Sprint(want) {
		t.Fatalf("slept %v, want %v", slept, want)
	}
}

func TestDo_BackoffCappedAtMaxDelay(t *testing.T) {
	var slept []time.Duration
	p := testPolicy(&slept)
	p.MaxAttempts = 8
	_, err := p.Do(context.Background(), func(context.Context) error { return &StatusError{Status: 500} })
	if err == nil {
		t.Fatal("want error after exhausting attempts")
	}
	if len(slept) != 7 {
		t.Fatalf("want 7 sleeps, got %d", len(slept))
	}
	for _, d := range slept {
		if d > time.Second {
			t.Fatalf("sleep %v exceeds MaxDelay", d)
		}
	}
}

func TestDo_DoesNotRetryClientErrors(t *testing.T) {
	var slept []time.Duration
	p := testPolicy(&slept)
	calls := 0
	attempts, err := p.Do(context.Background(), func(context.Context) error {
		calls++
		return &StatusError{Status: 400, Msg: "bad segment"}
	})
	if err == nil || attempts != 1 || calls != 1 || len(slept) != 0 {
		t.Fatalf("attempts=%d calls=%d slept=%v err=%v; want a single attempt", attempts, calls, slept, err)
	}
}

func TestDo_HonoursRetryAfterWithCap(t *testing.T) {
	var slept []time.Duration
	p := testPolicy(&slept)
	p.Jitter = func(int64) int64 { return 0 }
	calls := 0
	_, err := p.Do(context.Background(), func(context.Context) error {
		calls++
		switch calls {
		case 1:
			return &StatusError{Status: 429, RetryAfter: 2 * time.Second}
		case 2:
			return &StatusError{Status: 429, RetryAfter: time.Hour} // hostile value, capped
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{2 * time.Second, 3 * time.Second}
	if fmt.Sprint(slept) != fmt.Sprint(want) {
		t.Fatalf("slept %v, want %v", slept, want)
	}
}

func TestDo_StopsWhenSleepWouldOutliveDeadline(t *testing.T) {
	var slept []time.Duration
	p := testPolicy(&slept)
	p.BaseDelay = time.Minute
	p.MaxDelay = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	attempts, err := p.Do(ctx, func(context.Context) error { return &StatusError{Status: 503} })
	if attempts != 1 || err == nil || len(slept) != 0 {
		t.Fatalf("attempts=%d slept=%v err=%v; want to give up without sleeping", attempts, slept, err)
	}
}

func TestDo_PerAttemptTimeout(t *testing.T) {
	var slept []time.Duration
	p := testPolicy(&slept)
	p.MaxAttempts = 2
	p.PerAttempt = 10 * time.Millisecond
	calls := 0
	_, err := p.Do(context.Background(), func(ctx context.Context) error {
		calls++
		if calls == 1 {
			<-ctx.Done() // a hung platform call
			return ctx.Err()
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("calls=%d err=%v; want the hung attempt cut off and retried", calls, err)
	}
}

func TestDo_RealSleepRespectsCancel(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Hour, MaxDelay: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := p.Do(ctx, func(context.Context) error { return &StatusError{Status: 500} })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancel did not interrupt the backoff sleep")
	}
}

func TestDelay_FullJitterRange(t *testing.T) {
	p := DefaultRetryPolicy()
	for attempt := 1; attempt <= 10; attempt++ {
		for range 50 {
			d := p.Delay(attempt)
			if d < 0 || d > p.MaxDelay {
				t.Fatalf("Delay(%d) = %v out of range", attempt, d)
			}
		}
	}
}

func TestMock_FaultsAndIdempotentOps(t *testing.T) {
	m := NewMock()
	ctx := context.Background()
	s := domain.Segment{Device: "ctv", Geo: "US", Genre: "*", DemandPartner: "*"}
	f := domain.PlatformFloor{Segment: s, FloorMicros: 1_000_000, ManagedBy: domain.ManagedBy}

	m.FailNext("SetFloor", &StatusError{Status: 503})
	if err := m.SetFloor(ctx, "p", f); err == nil {
		t.Fatal("want injected fault")
	}
	for range 2 { // idempotent
		if err := m.SetFloor(ctx, "p", f); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := m.ListFloors(ctx, "p")
	if len(got) != 1 || got[0] != f {
		t.Fatalf("floors = %+v", got)
	}
	for range 2 { // deleting twice succeeds
		if err := m.DeleteFloor(ctx, "p", s); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := m.ListFloors(ctx, "p"); len(got) != 0 {
		t.Fatalf("want empty, got %+v", got)
	}
	if m.Calls("SetFloor") != 3 {
		t.Fatalf("SetFloor calls = %d, want 3", m.Calls("SetFloor"))
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.ListFloors(cctx, "p"); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
}
