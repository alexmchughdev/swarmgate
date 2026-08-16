package apply

import (
	"context"
	"slices"
	"sync"
)

// FakeApplier is an in-memory Applier for loop tests. It records applied
// changes in order and can inject a per-service error.
type FakeApplier struct {
	mu      sync.Mutex
	applied []Change
	errs    map[string]error
}

var _ Applier = (*FakeApplier)(nil)

// Fail makes subsequent Apply calls for the named service return err.
func (f *FakeApplier) Fail(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errs == nil {
		f.errs = make(map[string]error)
	}
	f.errs[name] = err
}

// Apply records the change, or returns the injected error without
// recording so Applied reflects only what actually took effect.
func (f *FakeApplier) Apply(_ context.Context, c Change) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs[c.Spec.Name]; err != nil {
		return err
	}
	f.applied = append(f.applied, c)
	return nil
}

// Applied returns a copy of the changes applied so far, in order.
func (f *FakeApplier) Applied() []Change {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.applied)
}
