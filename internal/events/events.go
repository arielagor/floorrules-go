// Package events carries domain events from the transactional outbox to
// consumers. Publisher is the seam a Pub/Sub or Kafka adapter would
// implement; Dispatcher is the in-process implementation used here.
//
// Delivery is at-least-once and durable up to the consumer: the relay marks
// an outbox row sent only after Publish returns nil, and Dispatcher returns
// nil only after every subscribed handler has succeeded. Until then the
// outbox row in the database is the copy of record, so a process that dies
// mid-delivery leaves the row to be claimed again when its lease expires.
// That durability is Postgres's: with STORE_BACKEND=memory the outbox, like
// everything else, is lost when the process stops.
package events

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/arielagor/floorrules-go/internal/domain"
	"github.com/arielagor/floorrules-go/internal/metrics"
	"github.com/arielagor/floorrules-go/internal/store"
)

// Message is one delivered event. Delivery is at-least-once: a consumer can
// see the same ID more than once and must dedupe.
type Message struct {
	ID      string
	Topic   string
	Payload []byte
}

// Handler processes a message. Returning an error asks for redelivery.
type Handler func(ctx context.Context, m Message) error

// Publisher delivers messages. Publish must return nil only once the message
// is safe: handled, for the in-process Dispatcher, or persisted and acked by
// the broker, for a Pub/Sub or Kafka adapter. Returning nil any earlier
// (handing it to an in-memory buffer, say) loses the event when the process
// stops, because the relay then marks the outbox row sent.
type Publisher interface {
	Publish(ctx context.Context, m Message) error
}

// Dispatcher is the in-process Publisher: Publish runs the topic's handlers
// in subscription order and returns the first error.
type Dispatcher struct {
	mu       sync.Mutex
	handlers map[string][]Handler
}

// NewDispatcher returns a Dispatcher with no subscribers.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{handlers: map[string][]Handler{}}
}

// Subscribe registers h for topic.
func (d *Dispatcher) Subscribe(topic string, h Handler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[topic] = append(d.handlers[topic], h)
}

// Publish implements Publisher. A handler that already succeeded runs again
// when the message is redelivered after a later handler fails, so every
// handler must be idempotent by message ID.
func (d *Dispatcher) Publish(ctx context.Context, m Message) error {
	d.mu.Lock()
	hs := append([]Handler(nil), d.handlers[m.Topic]...)
	d.mu.Unlock()
	for _, h := range hs {
		if err := h(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// Relay delivers committed outbox rows and records each outcome on the row.
// A row is marked sent only after Publish succeeds, so a crash in between
// causes a duplicate delivery, never a lost event; consumers dedupe by
// event ID. A failed delivery is retried with exponential backoff, and after
// MaxAttempts the row is dead-lettered in the database for an operator.
type Relay struct {
	Store store.Store
	Pub   Publisher
	Batch int
	// Lease is how long a claimed batch is reserved for this relay. The
	// relay stops starting new deliveries once the time left on the lease is
	// less than Timeout, so it never works on a row another relay may have
	// claimed since.
	Lease time.Duration
	// Timeout bounds one delivery (default 10s).
	Timeout time.Duration
	// MaxAttempts is the number of failed deliveries before a row is
	// dead-lettered (default 10).
	MaxAttempts int
	// BaseBackoff is the wait after the first failure, doubled after each
	// further one up to MaxBackoff (defaults 1s and 5m).
	BaseBackoff, MaxBackoff time.Duration
	Interval                time.Duration
	Log                     *slog.Logger
	Metrics                 *metrics.Registry
}

func (r *Relay) timeout() time.Duration { return orDefault(r.Timeout, 10*time.Second) }

func (r *Relay) maxAttempts() int {
	if r.MaxAttempts > 0 {
		return r.MaxAttempts
	}
	return 10
}

// backoff returns the wait before the next delivery after `failures`
// failed deliveries (failures >= 1).
func (r *Relay) backoff(failures int) time.Duration {
	base, ceiling := orDefault(r.BaseBackoff, time.Second), orDefault(r.MaxBackoff, 5*time.Minute)
	d := base
	for i := 1; i < failures && d < ceiling; i++ {
		d *= 2
	}
	return min(d, ceiling)
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// RunOnce claims one batch and delivers it. It returns how many events were
// delivered; the error is the first store error, if any. Delivery failures
// are recorded on their rows, not returned.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	claimed := time.Now()
	evs, err := r.Store.ClaimOutbox(ctx, r.Batch, r.Lease)
	if err != nil {
		return 0, err
	}
	delivered := 0
	for _, ev := range evs {
		if ctx.Err() != nil {
			break // shutting down: unclaimed rows return when the lease expires
		}
		if r.Lease > r.timeout() && time.Since(claimed) > r.Lease-r.timeout() {
			break // the rest go back to the pool when the lease expires
		}
		ok, err := r.deliver(ctx, ev)
		if err != nil {
			return delivered, err
		}
		if ok {
			delivered++
		}
	}
	return delivered, nil
}

// deliver publishes one event and records the outcome. It reports whether
// the event was delivered and returns only store errors.
func (r *Relay) deliver(ctx context.Context, ev domain.Event) (bool, error) {
	dctx, cancel := context.WithTimeout(ctx, r.timeout())
	pubErr := r.Pub.Publish(dctx, toMessage(ev))
	cancel()
	// Record the outcome even if shutdown has begun: the handler's work is
	// done (or failed) either way, and a short write now saves a duplicate
	// delivery or a lost attempt count later.
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer rcancel()
	if pubErr == nil {
		if err := r.Store.MarkOutboxSent(rctx, []string{ev.ID}); err != nil {
			return false, err
		}
		r.Metrics.Inc("outbox_published_total")
		return true, nil
	}
	if ctx.Err() != nil && errors.Is(pubErr, ctx.Err()) {
		// Cancelled by shutdown, not a failure of the event. Leave the row
		// claimed; it is delivered again when the lease expires.
		return false, nil
	}
	failures := ev.Attempts + 1
	dead, err := r.Store.MarkOutboxFailed(rctx, ev.ID, pubErr.Error(), r.backoff(failures), r.maxAttempts())
	if err != nil {
		return false, err
	}
	r.Metrics.Inc("outbox_delivery_failures_total", "topic", ev.Topic)
	if dead {
		r.Metrics.Inc("outbox_dead_lettered_total", "topic", ev.Topic)
		r.Log.Error("outbox event dead-lettered", "event_id", ev.ID, "topic", ev.Topic, "attempts", failures, "err", pubErr)
	} else {
		r.Log.Warn("outbox delivery failed, will retry", "event_id", ev.ID, "topic", ev.Topic,
			"attempts", failures, "retry_in", r.backoff(failures), "err", pubErr)
	}
	return false, nil
}

// RecordStats publishes the outbox backlog as gauges.
func (r *Relay) RecordStats(ctx context.Context) error {
	st, err := r.Store.OutboxStats(ctx)
	if err != nil {
		return err
	}
	r.Metrics.Set("outbox_pending", float64(st.Pending))
	r.Metrics.Set("outbox_oldest_pending_seconds", st.OldestPending.Seconds())
	r.Metrics.Set("outbox_dead_letters", float64(st.Dead))
	return nil
}

// Run relays on every tick until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
				r.Log.Warn("outbox relay", "delivered", n, "err", err)
			}
			if err := r.RecordStats(ctx); err != nil && ctx.Err() == nil {
				r.Log.Warn("outbox stats", "err", err)
			}
		}
	}
}

func toMessage(ev domain.Event) Message {
	return Message{ID: ev.ID, Topic: ev.Topic, Payload: ev.Payload}
}
