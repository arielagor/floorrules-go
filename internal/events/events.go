// Package events carries domain events between the transactional outbox and
// consumers. Publisher and Subscriber are the seam a Pub/Sub or Kafka adapter
// would implement; MemQueue is the in-process implementation used here.
package events

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/arielagor/floorrules/internal/domain"
	"github.com/arielagor/floorrules/internal/metrics"
	"github.com/arielagor/floorrules/internal/store"
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

// Publisher sends messages to a topic.
type Publisher interface {
	Publish(ctx context.Context, m Message) error
}

// ErrQueueFull is returned when the in-memory buffer is full; the relay
// leaves the outbox row unsent and retries on its next tick.
var ErrQueueFull = errors.New("queue full")

type delivery struct {
	msg      Message
	attempts int
}

// MemQueue is a bounded in-process queue with redelivery on handler error.
type MemQueue struct {
	ch          chan delivery
	mu          sync.Mutex
	handlers    map[string][]Handler
	maxAttempts int
	log         *slog.Logger
	deadLetters []Message
}

// NewMemQueue returns a queue with the given buffer size.
func NewMemQueue(buffer, maxAttempts int, log *slog.Logger) *MemQueue {
	return &MemQueue{
		ch: make(chan delivery, buffer), handlers: map[string][]Handler{},
		maxAttempts: max(maxAttempts, 1), log: log,
	}
}

// Subscribe registers h for topic. Call before Run.
func (q *MemQueue) Subscribe(topic string, h Handler) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.handlers[topic] = append(q.handlers[topic], h)
}

// Publish implements Publisher without blocking.
func (q *MemQueue) Publish(ctx context.Context, m Message) error {
	select {
	case q.ch <- delivery{msg: m}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrQueueFull
	}
}

// Run dispatches until ctx is cancelled.
func (q *MemQueue) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-q.ch:
			q.dispatch(ctx, d)
		}
	}
}

func (q *MemQueue) dispatch(ctx context.Context, d delivery) {
	q.mu.Lock()
	hs := q.handlers[d.msg.Topic]
	q.mu.Unlock()
	d.attempts++
	for _, h := range hs {
		if err := h(ctx, d.msg); err != nil {
			if d.attempts >= q.maxAttempts {
				q.log.Error("message dead-lettered", "event_id", d.msg.ID, "topic", d.msg.Topic, "attempts", d.attempts, "err", err)
				q.mu.Lock()
				q.deadLetters = append(q.deadLetters, d.msg)
				q.mu.Unlock()
				return
			}
			q.log.Warn("handler failed, redelivering", "event_id", d.msg.ID, "attempt", d.attempts, "err", err)
			select {
			case q.ch <- d:
			default:
				q.log.Error("redelivery dropped: queue full", "event_id", d.msg.ID)
			}
			return
		}
	}
}

// DeadLetters returns messages that exhausted their attempts.
func (q *MemQueue) DeadLetters() []Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]Message(nil), q.deadLetters...)
}

// Relay moves committed outbox rows onto the queue. It publishes first and
// marks sent second, so a crash in between causes a duplicate publish, never
// a lost event. Consumers dedupe by event ID.
type Relay struct {
	Store    store.Store
	Pub      Publisher
	Batch    int
	Lease    time.Duration
	Interval time.Duration
	Log      *slog.Logger
	Metrics  *metrics.Registry
}

// RunOnce relays one batch and returns how many events were published.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	evs, err := r.Store.ClaimOutbox(ctx, r.Batch, r.Lease)
	if err != nil {
		return 0, err
	}
	sent := make([]string, 0, len(evs))
	var pubErr error
	for _, ev := range evs {
		if pubErr = r.Pub.Publish(ctx, toMessage(ev)); pubErr != nil {
			break // remaining rows stay leased and are retried after the lease
		}
		sent = append(sent, ev.ID)
	}
	if err := r.Store.MarkOutboxSent(ctx, sent); err != nil {
		return 0, err
	}
	r.Metrics.Add("outbox_published_total", float64(len(sent)))
	return len(sent), pubErr
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
				r.Log.Warn("outbox relay", "published", n, "err", err)
			}
		}
	}
}

func toMessage(ev domain.Event) Message {
	return Message{ID: ev.ID, Topic: ev.Topic, Payload: ev.Payload}
}
