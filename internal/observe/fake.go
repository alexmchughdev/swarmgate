package observe

import (
	"context"
	"sync"

	"github.com/alexmchughdev/swarmgate/internal/spec"
)

// FakeObserver is an in-memory Observer for tests.
type FakeObserver struct {
	mu    sync.Mutex
	state spec.ObservedState
	err   error
}

var _ Observer = (*FakeObserver)(nil)

// Set replaces the state and error returned by subsequent Snapshot calls.
func (f *FakeObserver) Set(state spec.ObservedState, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = state
	f.err = err
}

// Snapshot returns the configured state and error.
func (f *FakeObserver) Snapshot(_ context.Context) (spec.ObservedState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.err
}
