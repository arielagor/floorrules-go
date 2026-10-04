package events

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arielagor/floorrules/internal/domain"
	"github.com/arielagor/floorrules/internal/id"
	"github.com/arielagor/floorrules/internal/metrics"
	"github.com/arielagor/floorrules/internal/store/memstore"
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

func TestRelay_PublishesAndMarksSent(t *testing.T) {
	st := memstore.New()
	seedEvents(t, st, 3)
	q := NewMemQueue(10, 1, quiet)
	m := metrics.New()
	r := &Relay{Store: st, Pub: q, Batch: 10, Lease: 0, Log: quiet, Metrics: m}
	n, err := r.RunOnce(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}
	if n, _ := r.RunOnce(context.Background()); n != 0 {
		t.Fatalf("sent events relayed again: %d", n)
	}
	if got := m.Get("outbox_published_total"); got != 3 {
		t.Fatalf("outbox_published_total = %v", got)
	}
}

func TestRelay_QueueFullLeavesRowsForRetry(t *testing.T) {
	st := memstore.New()
	seedEvents(t, st, 3)
	q := NewMemQueue(2, 1, quiet) // room for 2 of 3
	r := &Relay{Store: st, Pub: q, Batch: 10, Lease: 0, Log: quiet}
	n, err := r.RunOnce(context.Background())
	if !errors.Is(err, ErrQueueFull) || n != 2 {
		t.Fatalf("RunOnce = %d, %v; want 2 published then ErrQueueFull", n, err)
	}
	<-q.ch // consumer drains one
	n, err = r.RunOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("retry RunOnce = %d, %v; want the leftover event", n, err)
	}
}

func TestMemQueue_RedeliversThenDeadLetters(t *testing.T) {
	q := NewMemQueue(10, 3, quiet)
	var calls atomic.Int32
	done := make(chan struct{})
	q.Subscribe("t", func(context.Context, Message) error {
		if calls.Add(1) == 3 {
			defer close(done)
		}
		return errors.New("downstream down")
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	if err := q.Publish(ctx, Message{ID: "e1", Topic: "t"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler not retried 3 times")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(q.DeadLetters()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if dl := q.DeadLetters(); len(dl) != 1 || dl[0].ID != "e1" {
		t.Fatalf("dead letters = %+v", dl)
	}
	if calls.Load() != 3 {
		t.Fatalf("handler called %d times, want 3", calls.Load())
	}
}

func TestMemQueue_RedeliveryEventuallySucceeds(t *testing.T) {
	q := NewMemQueue(10, 5, quiet)
	var calls atomic.Int32
	ok := make(chan struct{})
	q.Subscribe("t", func(context.Context, Message) error {
		if calls.Add(1) < 2 {
			return errors.New("transient")
		}
		close(ok)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	_ = q.Publish(ctx, Message{ID: "e1", Topic: "t"})
	select {
	case <-ok:
	case <-time.After(5 * time.Second):
		t.Fatal("message never succeeded")
	}
	if len(q.DeadLetters()) != 0 {
		t.Fatal("succeeded message was dead-lettered")
	}
}

func TestRelay_RunStopsOnCancel(t *testing.T) {
	st := memstore.New()
	seedEvents(t, st, 1)
	q := NewMemQueue(10, 1, quiet)
	r := &Relay{Store: st, Pub: q, Batch: 10, Lease: time.Minute, Interval: 5 * time.Millisecond, Log: quiet}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { r.Run(ctx); close(stopped) }()
	select {
	case <-q.ch:
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
