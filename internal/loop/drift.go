package loop

import (
	"sync"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/diff"
	"github.com/alexmchughdev/swarmgate/internal/telemetry"
)

// cycleState carries the memory drift attribution needs across cycles: the
// last commit reconciled, the previous cycle's start time, and pending
// event hints. Created once per Run (or per RunOnce call, which never
// attributes drift since there is no prior cycle to compare against).
type cycleState struct {
	mu             sync.Mutex
	lastCommit     string
	lastCycleStart time.Time
	hints          map[string]time.Time
}

func newCycleState() *cycleState {
	return &cycleState{}
}

// sameCommitAsLast reports whether sha matches the previously reconciled
// commit. An empty lastCommit (no previous cycle yet) never matches, so the
// first cycle is never mistaken for drift.
func (st *cycleState) sameCommitAsLast(sha string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.lastCommit != "" && st.lastCommit == sha
}

func (st *cycleState) setLastCommit(sha string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.lastCommit = sha
}

// recordHint notes that an engine event touched service, for the next
// drift-attribution pass to pick up.
func (st *cycleState) recordHint(service string, at time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.hints == nil {
		st.hints = make(map[string]time.Time)
	}
	st.hints[service] = at
}

// beginCycle records the start of a new cycle and returns the previous
// cycle's start time, the freshness cutoff for this cycle's hints. Zero on
// the first cycle, so every pending hint counts as fresh.
func (st *cycleState) beginCycle(now time.Time) time.Time {
	st.mu.Lock()
	defer st.mu.Unlock()
	prev := st.lastCycleStart
	st.lastCycleStart = now
	return prev
}

// takeHint reports whether a fresh hint is pending for service and clears
// it either way. Fresh means it arrived after since (the previous cycle's
// start); an older, unconsumed hint is expired rather than left to
// mislabel a later drift as event-driven.
func (st *cycleState) takeHint(service string, since time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	at, ok := st.hints[service]
	delete(st.hints, service)
	return ok && at.After(since)
}

// driftKindForUpdate derives the fixed-schema drift kind for a service
// update from its changed fields, in the differ's own priority order
// (image, replicas, env). Updates whose only changes are outside that set
// (labels, networks, ports, healthcheck) have no representable drift kind
// and are not reported — a known gap.
func driftKindForUpdate(changed []string) (string, bool) {
	for _, c := range changed {
		switch c {
		case "image", "replicas", "env":
			return c, true
		}
	}
	return "", false
}

// emitDrift reports one drift event per service touched by d, attributing
// each to the engine event that woke the cycle (if any arrived since it was
// last consumed) or to the ordinary poll otherwise. Kind reflects which
// side of the diff the service fell on: creates are services desired but
// missing from the cluster ("removed"), updates carry the differing field,
// and removes are services the cluster still has but the source no longer
// wants ("unmanaged").
func emitDrift(d diff.Diff, st *cycleState, emit func(telemetry.Event), since time.Time) {
	for _, s := range d.Creates {
		emitDriftEvent(emit, st, s.Name, "removed", since)
	}
	for _, u := range d.Updates {
		kind, ok := driftKindForUpdate(u.Changed)
		if !ok {
			continue
		}
		emitDriftEvent(emit, st, u.Name, kind, since)
	}
	for _, s := range d.Removes {
		emitDriftEvent(emit, st, s.Name, "unmanaged", since)
	}
}

func emitDriftEvent(emit func(telemetry.Event), st *cycleState, service, kind string, since time.Time) {
	origin := "poll"
	if st.takeHint(service, since) {
		origin = "event"
	}
	emit(telemetry.Event{
		Stage: telemetry.StageDrift, Service: service,
		Fields: map[string]any{"origin": origin, "kind": kind, "service": service},
	})
}
