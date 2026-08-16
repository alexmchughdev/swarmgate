package observe

import (
	"context"
	"log"
	"runtime/debug"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"

	"github.com/alexmchughdev/swarmgate/internal/spec"
)

// DriftHint names a managed service that an engine event touched, and when.
// It carries no classification of what changed: the loop re-diffs on its
// next cycle and attributes drift there (see loop.go). A hint exists only
// to shorten the poll_interval wait when events.wake is enabled.
type DriftHint struct {
	Service string
	At      time.Time
}

// Events subscribes to Docker service events and returns a channel of
// DriftHint, one per event that touches a managed service. The channel
// closes only when ctx is cancelled; a broken stream reconnects with
// backoff instead of closing the channel, so callers can range over it for
// the observer's lifetime.
func (o *SwarmObserver) Events(ctx context.Context) <-chan DriftHint {
	out := make(chan DriftHint)
	go o.runEvents(ctx, out)
	return out
}

func (o *SwarmObserver) runEvents(ctx context.Context, out chan<- DriftHint) {
	defer close(out)
	backoff := o.eventsBackoffMin
	for {
		start := o.now()
		o.streamEventsRecovered(ctx, out)
		if ctx.Err() != nil {
			return
		}
		// A connection that stayed up for a full backoff window is judged
		// stable: forgive prior failures and retry at the floor delay
		// instead of compounding backoff across unrelated outages.
		if o.now().Sub(start) >= backoff {
			backoff = o.eventsBackoffMin
		} else {
			backoff *= 2
			if backoff > o.eventsBackoffMax {
				backoff = o.eventsBackoffMax
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// streamEventsRecovered runs streamEvents with panic recovery: this
// background goroutine runs for the observer's whole lifetime, and an
// unrecovered panic anywhere in Go crashes the entire process — an
// unexpected Docker event message shape must not take the whole daemon
// down along with it. A recovered panic is treated exactly like the
// stream ending on its own: runEvents' existing backoff-then-reconnect
// loop picks it back up.
func (o *SwarmObserver) streamEventsRecovered(ctx context.Context, out chan<- DriftHint) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("swarmgate: recovered panic in events stream: %v\n%s", r, debug.Stack())
		}
	}()
	o.streamEvents(ctx, out)
}

// streamEvents reads one event subscription until it errors or ctx is
// cancelled.
func (o *SwarmObserver) streamEvents(ctx context.Context, out chan<- DriftHint) {
	msgs, errs := o.api.Events(ctx, events.ListOptions{
		Filters: filters.NewArgs(filters.Arg("type", string(events.ServiceEventType))),
	})
	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-errs:
			if !ok || err != nil {
				return
			}
		case msg, ok := <-msgs:
			if !ok {
				return
			}
			hint, matched := o.hintFor(ctx, msg)
			if !matched {
				continue
			}
			select {
			case out <- hint:
			case <-ctx.Done():
				return
			}
		}
	}
}

// hintFor decides whether msg concerns a managed service and, if so, the
// hint to emit for it. It prefers the label carried on the event itself;
// only when the event omits attributes entirely does it fall back to an
// inspect.
func (o *SwarmObserver) hintFor(ctx context.Context, msg events.Message) (DriftHint, bool) {
	name := msg.Actor.Attributes["name"]
	if name == "" {
		name = msg.Actor.ID
	}

	if managed, ok := msg.Actor.Attributes[spec.ManagedLabel]; ok {
		return DriftHint{Service: name, At: o.now()}, managed == "true"
	}

	svc, _, err := o.api.ServiceInspectWithRaw(ctx, msg.Actor.ID, swarm.ServiceInspectOptions{})
	if err != nil {
		// A remove event's service is already gone by the time we look it
		// up: that not-found is exactly the removal we want to surface,
		// so it is tolerated as a hint rather than dropped. Any other
		// inspect error leaves managed status unknown; the poll cycle
		// will catch a real change regardless, so the event is skipped
		// rather than guessed at.
		if cerrdefs.IsNotFound(err) && name != "" {
			return DriftHint{Service: name, At: o.now()}, true
		}
		return DriftHint{}, false
	}
	if svc.Spec.Annotations.Labels[spec.ManagedLabel] != "true" {
		return DriftHint{}, false
	}
	return DriftHint{Service: svc.Spec.Annotations.Name, At: o.now()}, true
}
