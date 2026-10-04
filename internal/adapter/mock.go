package adapter

import (
	"context"
	"sync"

	"github.com/arielagor/floorrules/internal/domain"
)

// Mock is an in-memory SSP with scripted faults. It is safe for concurrent use.
type Mock struct {
	mu     sync.Mutex
	floors map[string]map[string]domain.PlatformFloor // publisher -> segment key -> floor
	faults map[string][]error                         // method -> errors to return next, in order
	calls  map[string]int
}

// NewMock returns an empty platform.
func NewMock() *Mock {
	return &Mock{
		floors: map[string]map[string]domain.PlatformFloor{},
		faults: map[string][]error{},
		calls:  map[string]int{},
	}
}

// Seed writes a floor directly, bypassing faults. Tests use it to set up
// platform state or to simulate someone else changing the platform (drift).
func (m *Mock) Seed(publisherID string, f domain.PlatformFloor) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.put(publisherID, f)
}

// FailNext queues errors for the named method ("ListFloors", "SetFloor",
// "DeleteFloor"). Each call pops one; once the queue is empty calls succeed.
func (m *Mock) FailNext(method string, errs ...error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.faults[method] = append(m.faults[method], errs...)
}

// Calls reports how many times a method was invoked, faults included.
func (m *Mock) Calls(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[method]
}

func (m *Mock) enter(ctx context.Context, method string) error {
	m.calls[method]++
	if err := ctx.Err(); err != nil {
		return err
	}
	if q := m.faults[method]; len(q) > 0 {
		m.faults[method] = q[1:]
		return q[0]
	}
	return nil
}

func (m *Mock) put(publisherID string, f domain.PlatformFloor) {
	pub := m.floors[publisherID]
	if pub == nil {
		pub = map[string]domain.PlatformFloor{}
		m.floors[publisherID] = pub
	}
	pub[f.Segment.Key()] = f
}

// ListFloors implements SSP.
func (m *Mock) ListFloors(ctx context.Context, publisherID string) ([]domain.PlatformFloor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter(ctx, "ListFloors"); err != nil {
		return nil, err
	}
	out := make([]domain.PlatformFloor, 0, len(m.floors[publisherID]))
	for _, f := range m.floors[publisherID] {
		out = append(out, f)
	}
	return out, nil
}

// SetFloor implements SSP.
func (m *Mock) SetFloor(ctx context.Context, publisherID string, f domain.PlatformFloor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter(ctx, "SetFloor"); err != nil {
		return err
	}
	m.put(publisherID, f)
	return nil
}

// DeleteFloor implements SSP. Deleting a missing floor succeeds.
func (m *Mock) DeleteFloor(ctx context.Context, publisherID string, s domain.Segment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter(ctx, "DeleteFloor"); err != nil {
		return err
	}
	delete(m.floors[publisherID], s.Key())
	return nil
}
