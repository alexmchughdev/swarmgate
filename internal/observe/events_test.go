package observe

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"

	"github.com/alexmchughdev/swarmgate/internal/spec"
)

// eventsAttempt scripts one Events subscription: the messages it delivers
// and, optionally, the terminal error that ends it. With no error the
// subscription blocks (like a live, idle connection) until ctx is
// cancelled, mirroring the real client's contract.
type eventsAttempt struct {
	msgs []events.Message
	err  error
}

// eventsFake serves one scripted attempt per call to Events, mirroring the
// real client: only the error channel is closed, and exactly one value is
// sent on it before that happens.
type eventsFake struct {
	mu       sync.Mutex
	attempts []eventsAttempt
	calls    int32

	inspectSvc swarm.Service
	inspectErr error
}

func (f *eventsFake) Events(ctx context.Context, _ events.ListOptions) (<-chan events.Message, <-chan error) {
	i := int(atomic.AddInt32(&f.calls, 1)) - 1
	f.mu.Lock()
	var attempt eventsAttempt
	if i < len(f.attempts) {
		attempt = f.attempts[i]
	}
	f.mu.Unlock()

	msgs := make(chan events.Message)
	errs := make(chan error, 1)
	go func() {
		defer close(errs)
		for _, m := range attempt.msgs {
			select {
			case msgs <- m:
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			}
		}
		if attempt.err != nil {
			errs <- attempt.err
			return
		}
		<-ctx.Done()
		errs <- ctx.Err()
	}()
	return msgs, errs
}

func (f *eventsFake) callCount() int {
	return int(atomic.LoadInt32(&f.calls))
}

func (f *eventsFake) ServiceList(context.Context, swarm.ServiceListOptions) ([]swarm.Service, error) {
	return nil, nil
}

func (f *eventsFake) NetworkList(context.Context, network.ListOptions) ([]network.Summary, error) {
	return nil, nil
}

func (f *eventsFake) ServiceInspectWithRaw(context.Context, string, swarm.ServiceInspectOptions) (swarm.Service, []byte, error) {
	return f.inspectSvc, nil, f.inspectErr
}

func (f *eventsFake) TaskList(context.Context, swarm.TaskListOptions) ([]swarm.Task, error) {
	return nil, nil
}

func newEventsTestObserver(fake *eventsFake) *SwarmObserver {
	return &SwarmObserver{
		api:              fake,
		eventsBackoffMin: time.Millisecond,
		eventsBackoffMax: 5 * time.Millisecond,
		now:              time.Now,
	}
}

func recvHint(t *testing.T, ch <-chan DriftHint, timeout time.Duration) (DriftHint, bool) {
	t.Helper()
	select {
	case h, ok := <-ch:
		return h, ok
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a hint")
		return DriftHint{}, false
	}
}

func assertNoHint(t *testing.T, ch <-chan DriftHint, window time.Duration) {
	t.Helper()
	select {
	case h, ok := <-ch:
		t.Fatalf("unexpected hint %+v (open=%v)", h, ok)
	case <-time.After(window):
	}
}

func TestEventsEmitsHintForManagedServiceViaAttribute(t *testing.T) {
	fake := &eventsFake{attempts: []eventsAttempt{{
		msgs: []events.Message{{
			Type: events.ServiceEventType,
			Actor: events.Actor{
				ID:         "svc-1",
				Attributes: map[string]string{"name": "web_nginx", spec.ManagedLabel: "true"},
			},
		}},
	}}}
	o := newEventsTestObserver(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := o.Events(ctx)
	hint, ok := recvHint(t, ch, time.Second)
	if !ok || hint.Service != "web_nginx" {
		t.Fatalf("hint = %+v, ok=%v", hint, ok)
	}
	if hint.At.IsZero() {
		t.Fatal("hint.At not set")
	}
}

func TestEventsSkipsUnmanagedViaAttribute(t *testing.T) {
	fake := &eventsFake{attempts: []eventsAttempt{{
		msgs: []events.Message{{
			Type: events.ServiceEventType,
			Actor: events.Actor{
				ID:         "svc-1",
				Attributes: map[string]string{"name": "other_svc", spec.ManagedLabel: "false"},
			},
		}},
	}}}
	o := newEventsTestObserver(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := o.Events(ctx)
	assertNoHint(t, ch, 30*time.Millisecond)
}

func TestEventsFallsBackToInspectWhenAttributesMissing(t *testing.T) {
	fake := &eventsFake{
		attempts: []eventsAttempt{{
			msgs: []events.Message{{
				Type:  events.ServiceEventType,
				Actor: events.Actor{ID: "svc-1"},
			}},
		}},
		inspectSvc: swarm.Service{Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{
				Name:   "web_nginx",
				Labels: map[string]string{spec.ManagedLabel: "true"},
			},
		}},
	}
	o := newEventsTestObserver(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := o.Events(ctx)
	hint, ok := recvHint(t, ch, time.Second)
	if !ok || hint.Service != "web_nginx" {
		t.Fatalf("hint = %+v, ok=%v", hint, ok)
	}
}

func TestEventsFallbackSkipsUnmanagedService(t *testing.T) {
	fake := &eventsFake{
		attempts: []eventsAttempt{{
			msgs: []events.Message{{
				Type:  events.ServiceEventType,
				Actor: events.Actor{ID: "svc-1"},
			}},
		}},
		inspectSvc: swarm.Service{Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{Name: "other_svc"},
		}},
	}
	o := newEventsTestObserver(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := o.Events(ctx)
	assertNoHint(t, ch, 30*time.Millisecond)
}

func TestEventsToleratesNotFoundOnFallbackInspect(t *testing.T) {
	fake := &eventsFake{
		attempts: []eventsAttempt{{
			msgs: []events.Message{{
				Type:  events.ServiceEventType,
				Actor: events.Actor{ID: "svc-1", Attributes: map[string]string{"name": "web_removed"}},
			}},
		}},
		inspectErr: errNotFound("svc-1"),
	}
	o := newEventsTestObserver(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := o.Events(ctx)
	hint, ok := recvHint(t, ch, time.Second)
	if !ok || hint.Service != "web_removed" {
		t.Fatalf("removal hint not tolerated: hint = %+v, ok=%v", hint, ok)
	}
}

func TestEventsSkipsOnOtherFallbackInspectError(t *testing.T) {
	fake := &eventsFake{
		attempts: []eventsAttempt{{
			msgs: []events.Message{{
				Type:  events.ServiceEventType,
				Actor: events.Actor{ID: "svc-1", Attributes: map[string]string{"name": "web_x"}},
			}},
		}},
		inspectErr: errors.New("connection reset"),
	}
	o := newEventsTestObserver(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := o.Events(ctx)
	assertNoHint(t, ch, 30*time.Millisecond)
}

func TestEventsReconnectsAfterError(t *testing.T) {
	fake := &eventsFake{attempts: []eventsAttempt{
		{err: errors.New("stream reset")},
		{msgs: []events.Message{{
			Type: events.ServiceEventType,
			Actor: events.Actor{
				ID:         "svc-1",
				Attributes: map[string]string{"name": "web_nginx", spec.ManagedLabel: "true"},
			},
		}}},
	}}
	o := newEventsTestObserver(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := o.Events(ctx)
	hint, ok := recvHint(t, ch, time.Second)
	if !ok || hint.Service != "web_nginx" {
		t.Fatalf("hint after reconnect = %+v, ok=%v", hint, ok)
	}
	if got := fake.callCount(); got < 2 {
		t.Fatalf("Events called %d times, want at least 2 (reconnect)", got)
	}
}

func TestEventsClosesOnCancel(t *testing.T) {
	fake := &eventsFake{attempts: []eventsAttempt{{}}} // no messages, no error: idle live connection
	o := newEventsTestObserver(fake)
	ctx, cancel := context.WithCancel(context.Background())

	ch := o.Events(ctx)
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to close on cancel, got a hint instead")
		}
	case <-time.After(time.Second):
		t.Fatal("channel did not close within 1s of cancellation")
	}
}

// panickyEventsAPI panics on its first Events() call, then serves
// eventsFake's normal scripted behavior — simulating an unexpected
// SDK/response shape crashing mid-stream, once.
type panickyEventsAPI struct {
	eventsFake
	calls int32
}

func (f *panickyEventsAPI) Events(ctx context.Context, opts events.ListOptions) (<-chan events.Message, <-chan error) {
	if atomic.AddInt32(&f.calls, 1) == 1 {
		panic("simulated panic in Docker events client")
	}
	return f.eventsFake.Events(ctx, opts)
}

// TestStreamEventsRecoversPanic is the production-hardening regression
// test: this goroutine runs for the observer's whole lifetime, and an
// unrecovered panic anywhere in Go crashes the entire daemon, not just
// this one background stream. If recovery didn't work, this test process
// itself would crash rather than report a failure.
func TestStreamEventsRecoversPanic(t *testing.T) {
	// The panicking wrapper's own call counter is independent of the
	// embedded eventsFake's: the first (panicking) call never reaches
	// eventsFake.Events at all, so the fake's own first served attempt is
	// this one, not a second entry.
	fake := &panickyEventsAPI{eventsFake: eventsFake{attempts: []eventsAttempt{
		{msgs: []events.Message{{
			Type: events.ServiceEventType,
			Actor: events.Actor{
				ID:         "svc-1",
				Attributes: map[string]string{"name": "web_nginx", spec.ManagedLabel: "true"},
			},
		}}},
	}}}
	o := newEventsTestObserver(&fake.eventsFake)
	o.api = fake // use the panicking wrapper as the API, not the plain fake

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := o.Events(ctx)
	hint, ok := recvHint(t, ch, time.Second)
	if !ok || hint.Service != "web_nginx" {
		t.Fatalf("hint after recovered panic = %+v, ok=%v", hint, ok)
	}
	if got := atomic.LoadInt32(&fake.calls); got < 2 {
		t.Fatalf("Events called %d times, want at least 2 (the panicking attempt plus the recovered reconnect)", got)
	}
}
