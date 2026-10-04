package events

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arielagor/floorrules-go/internal/domain"
	"github.com/arielagor/floorrules-go/internal/id"
	"github.com/arielagor/floorrules-go/internal/metrics"
	"github.com/arielagor/floorrules-go/internal/store/memstore"
)

var quiet = slog.New(slog.DiscardHandler)

func seedEvents(t *testing.T, st *memstore.Store, n int) {
	t.Helper()
	for i := range n {
		_, err := st.CreateRule(context.Background(), domain.Rule{
			ID: id.New(), PublisherID: "p", FloorMicros: 1_000_000, Currency: "USD", CreatedBy: "t",
			Segment: domain.Segment{Device: "ctv", Geo: []string{"US", "CA", "GB", "DE"}[i], Genre: "*", DemandPartner: "*"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// pipeline wires a relay to a consumer the way cmd/floorsvc does.
type pipeline struct {
	relay *Relay
}

func newPipeline(st *memstore.Store, h Handler) pipeline {
	d := NewDispatcher()
	d.Subscribe(domain.TopicRuleChanged, h)
	return pipeline{relay: &Relay{Store: st, Pub: d, Batch: 10, Lease: 0, Log: quiet}}
}

// deliver relays once; delivery is synchronous.
func (p pipeline) deliver(ctx context.Context) {
	_, _ = p.relay.RunOnce(ctx)
}

// M4 (day-2 review): the relay marked a row sent as soon as the in-memory
// queue accepted it. A process that stopped before its consumer finished
// (every rolling deploy) lost the event for good.
func TestRelay_EventSurvivesRestartBeforeHandling(t *testing.T) {
	st := memstore.New()
	seedEvents(t, st, 1)

	// Process 1 picks the event up and is killed before its consumer is
	// done with it.
	ctx1, kill := context.WithCancel(context.Background())
	p1 := newPipeline(st, func(ctx context.Context, _ Message) error { kill(); return ctx.Err() })
	_, _ = p1.relay.RunOnce(ctx1)
	kill()

	// Process 2 starts on the same database with a healthy consumer.
	var handled atomic.Int32
	p2 := newPipeline(st, func(context.Context, Message) error { handled.Add(1); return nil })
	ctx2, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	p2.deliver(ctx2)
	if got := handled.Load(); got != 1 {
		t.Fatalf("event handled %d times after the restart, want 1: it was marked sent before its consumer ran and was lost with the process", got)
	}
}

func TestRelay_DeliversAndMarksSent(t *testing.T) {
	st := memstore.New()
	seedEvents(t, st, 3)
	m := metrics.New()
	d := NewDispatcher()
	var got []string
	d.Subscribe(domain.TopicRuleChanged, func(_ context.Context, msg Message) error {
		got = append(got, msg.ID)
		return nil
	})
	r := &Relay{Store: st, Pub: d, Batch: 10, Lease: time.Minute, Log: quiet, Metrics: m}
	n, err := r.RunOnce(context.Background())
	if err != nil || n != 3 || len(got) != 3 {
		t.Fatalf("RunOnce = %d, %v; handled %d", n, err, len(got))
	}
	st.SetClock(func() time.Time { return time.Now().Add(time.Hour) }) // past every lease
	if n, _ := r.RunOnce(context.Background()); n != 0 {
		t.Fatalf("delivered events relayed again: %d", n)
	}
	if v := m.Get("outbox_published_total"); v != 3 {
		t.Fatalf("outbox_published_total = %v", v)
	}
}

// A failed delivery stays in the outbox, waits out its backoff, and is
// delivered on a later pass; a 2-second outage no longer burns every attempt.
func TestRelay_FailedDeliveryIsRetriedAfterBackoff(t *testing.T) {
	st := memstore.New()
	seedEvents(t, st, 1)
	now := time.Now()
	st.SetClock(func() time.Time { return now })
	m := metrics.New()
	d := NewDispatcher()
	var calls atomic.Int32
	d.Subscribe(domain.TopicRuleChanged, func(context.Context, Message) error {
		if calls.Add(1) == 1 {
			return errors.New("ssp blip")
		}
		return nil
	})
	r := &Relay{Store: st, Pub: d, Batch: 10, Lease: time.Minute, BaseBackoff: 30 * time.Second, Log: quiet, Metrics: m}
	if n, err := r.RunOnce(context.Background()); n != 0 || err != nil {
		t.Fatalf("first pass = %d, %v; want a recorded failure", n, err)
	}
	if v := m.Get("outbox_delivery_failures_total", "topic", domain.TopicRuleChanged); v != 1 {
		t.Fatalf("outbox_delivery_failures_total = %v", v)
	}
	st.SetClock(func() time.Time { return now.Add(10 * time.Second) })
	if n, _ := r.RunOnce(context.Background()); n != 0 || calls.Load() != 1 {
		t.Fatalf("redelivered inside its backoff: n=%d calls=%d", n, calls.Load())
	}
	st.SetClock(func() time.Time { return now.Add(31 * time.Second) })
	if n, err := r.RunOnce(context.Background()); n != 1 || err != nil {
		t.Fatalf("retry after backoff = %d, %v", n, err)
	}
}

// After MaxAttempts failures the row is dead-lettered in the store, never
// claimed again, and counted.
func TestRelay_DeadLettersAfterMaxAttempts(t *testing.T) {
	st := memstore.New()
	seedEvents(t, st, 1)
	now := time.Now()
	m := metrics.New()
	d := NewDispatcher()
	var calls atomic.Int32
	d.Subscribe(domain.TopicRuleChanged, func(context.Context, Message) error {
		calls.Add(1)
		return errors.New("downstream down")
	})
	r := &Relay{Store: st, Pub: d, Batch: 10, Lease: time.Minute, MaxAttempts: 3, Log: quiet, Metrics: m}
	for i := range 5 {
		at := now.Add(time.Duration(i) * time.Hour) // past any backoff
		st.SetClock(func() time.Time { return at })
		if _, err := r.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("handler called %d times, want 3", calls.Load())
	}
	if v := m.Get("outbox_dead_lettered_total", "topic", domain.TopicRuleChanged); v != 1 {
		t.Fatalf("outbox_dead_lettered_total = %v", v)
	}
	if err := r.RecordStats(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Get("outbox_dead_letters") != 1 || m.Get("outbox_pending") != 0 {
		t.Fatalf("gauges: dead=%v pending=%v", m.Get("outbox_dead_letters"), m.Get("outbox_pending"))
	}
}

func TestRelay_BackoffDoublesToACeiling(t *testing.T) {
	r := &Relay{BaseBackoff: time.Second, MaxBackoff: 10 * time.Second}
	for failures, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 4: 8 * time.Second, 5: 10 * time.Second, 40: 10 * time.Second} {
		if got := r.backoff(failures); got != want {
			t.Errorf("backoff(%d) = %v, want %v", failures, got, want)
		}
	}
}

func TestRelay_StatsReportBacklogAge(t *testing.T) {
	st := memstore.New()
	seedEvents(t, st, 2)
	st.SetClock(func() time.Time { return time.Now().Add(90 * time.Second) })
	m := metrics.New()
	r := &Relay{Store: st, Log: quiet, Metrics: m}
	if err := r.RecordStats(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Get("outbox_pending") != 2 || m.Get("outbox_oldest_pending_seconds") < 89 {
		t.Fatalf("pending=%v oldest=%v", m.Get("outbox_pending"), m.Get("outbox_oldest_pending_seconds"))
	}
}

func TestDispatcher_StopsAtFirstFailingHandler(t *testing.T) {
	d := NewDispatcher()
	var second atomic.Bool
	d.Subscribe("t", func(context.Context, Message) error { return errors.New("boom") })
	d.Subscribe("t", func(context.Context, Message) error { second.Store(true); return nil })
	if err := d.Publish(context.Background(), Message{ID: "e1", Topic: "t"}); err == nil {
		t.Fatal("handler error not returned")
	}
	if second.Load() {
		t.Fatal("ran a later handler after an earlier one failed")
	}
	if err := d.Publish(context.Background(), Message{ID: "e2", Topic: "nobody"}); err != nil {
		t.Fatalf("topic with no subscribers: %v", err)
	}
}

func TestRelay_RunStopsOnCancel(t *testing.T) {
	st := memstore.New()
	seedEvents(t, st, 1)
	d := NewDispatcher()
	got := make(chan struct{}, 1)
	d.Subscribe(domain.TopicRuleChanged, func(context.Context, Message) error {
		select {
		case got <- struct{}{}:
		default:
		}
		return nil
	})
	r := &Relay{Store: st, Pub: d, Batch: 10, Lease: time.Minute, Interval: 5 * time.Millisecond, Log: quiet}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { r.Run(ctx); close(stopped) }()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("Run never relayed")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}
