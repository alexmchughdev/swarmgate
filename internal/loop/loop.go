package loop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/apply"
	"github.com/alexmchughdev/swarmgate/internal/config"
	"github.com/alexmchughdev/swarmgate/internal/diff"
	"github.com/alexmchughdev/swarmgate/internal/gate"
	"github.com/alexmchughdev/swarmgate/internal/observe"
	"github.com/alexmchughdev/swarmgate/internal/source"
	"github.com/alexmchughdev/swarmgate/internal/spec"
	"github.com/alexmchughdev/swarmgate/internal/telemetry"
)

// Source yields the desired-state input for one cycle.
type Source interface {
	Fetch(ctx context.Context) (source.Commit, []source.StackFile, error)
}

// Observer snapshots the live cluster state.
type Observer interface {
	Snapshot(ctx context.Context) (spec.ObservedState, error)
}

// Converger blocks until the cluster settles on the applied diff.
type Converger interface {
	AwaitConverged(ctx context.Context, d diff.Diff, timeout time.Duration) error
}

// Parser turns stack files into desired state.
type Parser func(files []source.StackFile) (spec.DesiredState, error)

// Resolver pins desired images to digests; registry auth is bound by the
// caller.
type Resolver func(ctx context.Context, d *spec.DesiredState) error

// EventSource yields hints that an engine event touched a managed service.
// It shortens the poll wait when events.wake is enabled and feeds drift
// attribution's event-vs-poll origin. Nil disables both.
type EventSource func(ctx context.Context) <-chan observe.DriftHint

// NopConverger returns immediately without waiting; it stands in where no
// convergence wait is wanted, such as tests.
type NopConverger struct{}

// AwaitConverged reports immediate convergence.
func (NopConverger) AwaitConverged(context.Context, diff.Diff, time.Duration) error { return nil }

// Deps are the seams the loop drives; tests substitute hand-written fakes.
type Deps struct {
	Source    Source
	Parse     Parser
	Resolve   Resolver
	Observer  Observer
	Applier   apply.Applier
	Converger Converger
	Gate      gate.Gate
	Events    EventSource
	Recorder  telemetry.Recorder
	Log       *slog.Logger
	Now       func() time.Time
	// After computes the pre-apply-failure backoff wait; matches
	// time.After's signature so tests can substitute an instant or
	// call-recording fake without real sleeps. Nil defaults to time.After.
	After func(time.Duration) <-chan time.Time
}

// Run executes reconcile cycles until ctx is cancelled: one cycle
// immediately on start, then one per poll tick (or sooner, on an engine
// event when events.wake is enabled). Cycle errors are logged and the loop
// keeps going; only ctx cancellation ends it.
func Run(ctx context.Context, deps Deps, cfg config.Config) error {
	wake := make(chan struct{}, 1)
	st := newCycleState()
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				requestWake(wake)
			}
		}
	}()
	if cfg.Events.Wake && deps.Events != nil {
		go watchEvents(ctx, deps.Events, st, wake, deps.Log, deps.After)
	}
	return run(ctx, deps, cfg, wake, st)
}

// watchEventsReconnectBackoff bounds the delay before watchEvents opens a
// fresh src(ctx) subscription after the previous one ended, whether by a
// clean channel close or a recovered panic.
const watchEventsReconnectBackoff = time.Second

// watchEvents records each hint's arrival for drift attribution and
// coalesces a wake, shortening the poll wait for the cycle that will
// observe it. Runs for the process's whole lifetime in its own goroutine.
// Mirrors internal/observe's own runEvents/streamEventsRecovered pattern:
// a subscription that ends, whether the channel closed on its own or a
// panic was recovered from, is reopened after a short backoff rather than
// left dead. Without this, a single panic in this loop's own bookkeeping
// (not src(ctx), which already reconnects internally on stream/network
// failures) permanently disabled event-driven wake for the rest of the
// process's life, with no way back short of a restart.
func watchEvents(ctx context.Context, src EventSource, st *cycleState, wake chan<- struct{}, log *slog.Logger, after func(time.Duration) <-chan time.Time) {
	if after == nil {
		after = time.After
	}
	for {
		watchEventsOnce(ctx, src, st, wake, log)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-after(watchEventsReconnectBackoff):
		}
	}
}

// watchEventsOnce runs one src(ctx) subscription with panic recovery. A
// recovered panic leaks whatever goroutine src(ctx) started for this
// subscription — the same accepted tradeoff as the gate's trusted-root
// fetch timeout: panics are exceptional, and surviving one matters more
// than perfect cleanup on that rare path.
func watchEventsOnce(ctx context.Context, src EventSource, st *cycleState, wake chan<- struct{}, log *slog.Logger) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("recovered panic watching engine events", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	for hint := range src(ctx) {
		st.recordHint(hint.Service, hint.At)
		requestWake(wake)
	}
}

// RunOnce executes exactly one cycle. A non-nil error means the cycle did
// not fully reconcile (stage failure or any per-service apply failure). A
// one-shot run has no prior cycle to compare against, so it never
// attributes drift — every non-empty diff is treated as deployment.
func RunOnce(ctx context.Context, deps Deps, cfg config.Config) error {
	err := safeCycle(ctx, deps, cfg, newCycleState())
	var lastWarned uint64
	warnDropped(deps, &lastWarned)
	return err
}

// safeCycle runs cycle with panic recovery. This daemon runs continuously
// and manages every stack a single instance is pointed at; an unhandled
// panic anywhere in one cycle — a malformed stack file, an unexpected
// Docker SDK response shape, any edge case that wasn't anticipated — must
// not crash the whole process and stop reconciling every other stack
// along with it. A recovered panic is treated as a preApplyError so
// run()'s existing backoff applies to it exactly like an unreachable
// registry would: something is badly wrong, and hammering the next tick
// immediately is the wrong response.
func safeCycle(ctx context.Context, deps Deps, cfg config.Config, st *cycleState) (err error) {
	// Generated here, not inside cycle, so a recovered panic's error event
	// carries the same run_id as the stage events the same cycle already
	// emitted, instead of a fresh one orphaned from them.
	runID := telemetry.NewRunID()
	defer func() {
		if r := recover(); r != nil {
			deps.Log.Error("reconcile cycle panicked", "panic", r, "stack", string(debug.Stack()))
			err = preApplyErr(func(e telemetry.Event) {
				e.RunID = runID
				deps.Recorder.Emit(e)
			}, "panic", fmt.Errorf("%v", r))
		}
	}()
	return cycle(ctx, deps, cfg, st, runID)
}

// requestWake sets the dirty flag: the buffer of one coalesces any number
// of ticks or hints arriving during a cycle into a single immediate rerun.
func requestWake(wake chan<- struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// run is the wake-driven core, split from Run so tests can drive the wake
// channel directly. A pre-apply cycle failure (source/registry/Docker
// unreachable, before any change was attempted) backs off exponentially
// instead of waiting for the next normal wake; any other failure (partial
// apply, unconverged) keeps the prior behavior of retrying on the normal
// poll/event cadence, since those already carry their own "believed"
// telemetry describing exactly what state was reached.
func run(ctx context.Context, deps Deps, cfg config.Config, wake chan struct{}, st *cycleState) error {
	after := deps.After
	if after == nil {
		after = time.After
	}
	bo := newBackoff()
	var lastWarned uint64
	for {
		err := safeCycle(ctx, deps, cfg, st)
		warnDropped(deps, &lastWarned)
		switch {
		case err == nil:
			bo.reset()
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			deps.Log.Error("reconcile cycle failed", "error", err)
			var pae *preApplyError
			if errors.As(err, &pae) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-after(bo.next()):
				}
				continue
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		}
	}
}

// warnDropped surfaces telemetry loss: a run whose recorder dropped events
// is invalid, and that must be visible from the operational log.
// Warns once when drops first appear and again only when the count rises
// further, not on every subsequent cycle for the rest of the process's
// life with an unchanged count.
func warnDropped(deps Deps, lastWarned *uint64) {
	if r, ok := deps.Recorder.(interface{ Dropped() uint64 }); ok {
		if n := r.Dropped(); n > *lastWarned {
			deps.Log.Warn("telemetry events dropped; JSONL stream is incomplete", "count", n)
			*lastWarned = n
		}
	}
}

func cycle(ctx context.Context, deps Deps, cfg config.Config, st *cycleState, runID string) error {
	emit := func(e telemetry.Event) {
		e.RunID = runID
		deps.Recorder.Emit(e)
	}
	// cycleStart is converged's t0: its DurMS measures the whole cycle
	// from before poll, not just the convergence wait. See the DurMS
	// comment on telemetry.Event.
	cycleStart := deps.Now()
	// prevCycleStart bounds hint freshness for this cycle's drift
	// attribution: a hint older than the previous cycle's start is stale.
	prevCycleStart := st.beginCycle(cycleStart)

	t := deps.Now()
	pollCtx, cancel := context.WithTimeout(ctx, cfg.StageTimeout)
	commit, files, err := deps.Source.Fetch(pollCtx)
	cancel()
	if err != nil {
		return preApplyErr(emit, "poll", err)
	}
	// Compared before being overwritten: a non-empty diff against an
	// unchanged commit is drift, not deployment (see emitDrift below).
	sameCommit := st.sameCommitAsLast(commit.SHA)
	st.setLastCommit(commit.SHA)
	emit(telemetry.Event{
		Stage: telemetry.StagePoll, DurMS: ms(deps.Now().Sub(t)),
		Fields: map[string]any{"commit": commit.SHA},
	})

	t = deps.Now()
	desired, err := deps.Parse(files)
	if err != nil {
		return preApplyErr(emit, "parse", err)
	}
	emit(telemetry.Event{Stage: telemetry.StageParse, DurMS: ms(deps.Now().Sub(t))})

	// Pre-resolve references are captured so resolve events can report the
	// original image alongside the digest it was pinned to.
	preResolve := make(map[string]string, len(desired.Services))
	for name, s := range desired.Services {
		preResolve[name] = s.Image
	}
	t = deps.Now()
	resolveCtx, cancel := context.WithTimeout(ctx, cfg.StageTimeout)
	err = deps.Resolve(resolveCtx, &desired)
	cancel()
	if err != nil {
		return preApplyErr(emit, "resolve", err)
	}
	resolveDur := ms(deps.Now().Sub(t))
	for _, name := range slices.Sorted(maps.Keys(desired.Services)) {
		emit(telemetry.Event{
			Stage: telemetry.StageResolve, Service: name, DurMS: resolveDur,
			Fields: map[string]any{"image": preResolve[name], "digest": digestOf(desired.Services[name].Image)},
		})
	}

	t = deps.Now()
	observeCtx, cancel := context.WithTimeout(ctx, cfg.StageTimeout)
	observedState, err := deps.Observer.Snapshot(observeCtx)
	cancel()
	if err != nil {
		return preApplyErr(emit, "observe", err)
	}
	emit(telemetry.Event{Stage: telemetry.StageObserve, DurMS: ms(deps.Now().Sub(t))})

	d := diff.Compute(desired, observedState)
	emit(telemetry.Event{
		Stage: telemetry.StageDiff,
		Fields: map[string]any{
			"creates": len(d.Creates), "updates": len(d.Updates), "removes": len(d.Removes),
		},
	})
	// An empty diff ends the cycle with stage events only; converged is
	// reserved for cycles that changed something.
	if d.Empty() {
		return nil
	}

	if sameCommit {
		emitDrift(d, st, emit, prevCycleStart)
	}
	changes := plan(d, cfg.Prune)

	// Scaled by len(changes): gate.Verify checks every change in the
	// batch, potentially one registry/tlog round trip each, so a single
	// change's worth of stage timeout would be too tight the moment more
	// than one service needs verification in the same cycle.
	gateCtx, cancel := context.WithTimeout(ctx, cfg.StageTimeout*time.Duration(max(1, len(changes))))
	changes, allPassed, err := verify(gateCtx, deps.Gate, changes, emit, deps.Now)
	cancel()
	if err != nil {
		return preApplyErr(emit, "gate", err)
	}
	if !allPassed && cfg.Gate.Mode == config.GateModeAbortCycle {
		// Every verdict (pass and reject alike) was already emitted by
		// verify above; abort-cycle's distinguishing behavior is applying
		// nothing rather than the per-service subset that passed.
		emit(telemetry.Event{
			Stage:  telemetry.StageError,
			Fields: map[string]any{"believed": "aborted", "reason": "gate"},
		})
		return errors.New("gate: cycle aborted, one or more changes rejected")
	}
	// Every change was filtered out (gate-rejected, or pruning skipped
	// removes). Nothing was applied, so converged must not fire: the
	// cluster is still divergent.
	if len(changes) == 0 {
		return nil
	}

	var applied, pending []string
	appliedServices := make(map[string]bool)
	var lastErr error
	failedRecreates := make(map[string]bool)
	for _, c := range changes {
		t = deps.Now()
		if c.Action == apply.ActionCreate && c.Recreate && failedRecreates[c.Spec.Name] {
			err := errors.New("recreate skipped because service removal failed")
			emit(telemetry.Event{Stage: telemetry.StageApply, Service: c.Spec.Name, Fields: map[string]any{
				"action": string(c.Action), "outcome": "skipped", "error": err.Error(),
			}})
			continue
		}
		applyCtx, cancel := context.WithTimeout(ctx, cfg.StageTimeout)
		err := deps.Applier.Apply(applyCtx, c)
		cancel()
		fields := map[string]any{"action": string(c.Action), "outcome": "ok"}
		if err != nil {
			if c.Action == apply.ActionRemove && c.Recreate {
				failedRecreates[c.Spec.Name] = true
			}
			fields["outcome"] = "error"
			fields["error"] = err.Error()
		}
		emit(telemetry.Event{
			Stage: telemetry.StageApply, Service: c.Spec.Name,
			DurMS: ms(deps.Now().Sub(t)), Fields: fields,
		})
		if err != nil {
			// Per-service isolation: one failing service must not block
			// the rest of the cycle.
			deps.Log.Error("apply failed", "service", c.Spec.Name, "action", c.Action, "error", err)
			pending = append(pending, c.Spec.Name)
			lastErr = err
			continue
		}
		if c.Recreate {
			applied = append(applied, c.Spec.Name+":"+string(c.Action))
		} else {
			applied = append(applied, c.Spec.Name)
		}
		appliedServices[c.Spec.Name] = true
	}
	if len(pending) > 0 {
		// Record believed state so a mid-apply failure is
		// distinguishable from silent divergence in later analysis.
		emit(telemetry.Event{
			Stage: telemetry.StageError,
			Fields: map[string]any{
				"applied": applied, "pending": pending,
				"believed": "partial", "error": lastErr.Error(),
			},
		})
		return fmt.Errorf("apply: %d of %d changes failed: %w", len(pending), len(changes), lastErr)
	}

	// Convergence is awaited only on what was actually applied: changes
	// plan() excluded (pruning off) or verify() rejected (gate) were never
	// applied and would otherwise be waited on until timeout, defeating
	// per-service mode's "a reject blocks only that service" guarantee.
	if err := deps.Converger.AwaitConverged(ctx, changesToAwaitDiff(changes), cfg.ConvergeTimeout); err != nil {
		emit(telemetry.Event{
			Stage: telemetry.StageError,
			Fields: map[string]any{
				"applied": applied, "pending": []string{},
				"believed": "unconverged", "error": err.Error(),
			},
		})
		return fmt.Errorf("await converged: %w", err)
	}

	emit(telemetry.Event{
		Stage: telemetry.StageConverged, DurMS: ms(deps.Now().Sub(cycleStart)),
		Fields: map[string]any{"commit": commit.SHA, "services": len(appliedServices)},
	})
	return nil
}

// plan orders the diff into executable changes: creates, then updates, then
// removes (each already name-sorted by the differ). Removes are included
// only when pruning; drift telemetry for skipped or applied removes is
// emitDrift's responsibility, not plan's — it needs commit/hint state plan
// has no reason to see.
func plan(d diff.Diff, prune bool) []apply.Change {
	changes := make([]apply.Change, 0, len(d.Creates)+len(d.Updates)+len(d.Removes)+len(d.Updates))
	recreated := make(map[string]bool)
	// An attachment-only update cannot be applied in place on Swarm. Pair
	// its removal and replacement create, keeping them adjacent so the
	// apply loop can suppress the create if removal fails.
	for _, u := range d.Updates {
		if !attachmentOnlyUpdate(u) {
			continue
		}
		recreated[u.Name] = true
		changes = append(changes,
			apply.Change{Action: apply.ActionRemove, Spec: u.Old, Recreate: true},
			apply.Change{Action: apply.ActionCreate, Spec: u.New, Recreate: true},
		)
	}
	for _, s := range d.Creates {
		changes = append(changes, apply.Change{Action: apply.ActionCreate, Spec: s})
	}
	for _, u := range d.Updates {
		if recreated[u.Name] {
			continue
		}
		changes = append(changes, apply.Change{Action: apply.ActionUpdate, Spec: u.New})
	}
	if prune {
		for _, s := range d.Removes {
			changes = append(changes, apply.Change{Action: apply.ActionRemove, Spec: s})
		}
	}
	return changes
}

func attachmentOnlyUpdate(u diff.Change) bool {
	if len(u.Changed) == 0 {
		return false
	}
	for _, field := range u.Changed {
		if field != "configs" && field != "secrets" {
			return false
		}
	}
	return true
}

// changesToAwaitDiff rebuilds a diff.Diff from changes for AwaitConverged,
// which only ever reads a Change's Name and New spec — never Old — so
// reconstructing from the post-gate apply.Change list (rather than the
// pre-gate diff.Diff) loses nothing AwaitConverged needs while correctly
// excluding anything plan() or verify() decided not to apply.
func changesToAwaitDiff(changes []apply.Change) diff.Diff {
	var d diff.Diff
	for _, c := range changes {
		switch c.Action {
		case apply.ActionCreate:
			d.Creates = append(d.Creates, c.Spec)
		case apply.ActionUpdate:
			d.Updates = append(d.Updates, diff.Change{Name: c.Spec.Name, New: c.Spec})
		case apply.ActionRemove:
			if !c.Recreate {
				d.Removes = append(d.Removes, c.Spec)
			}
		}
	}
	return d
}

// verify runs changes through g, emitting one verify telemetry event per
// verdict (pass and reject alike, regardless of gate.mode — the event
// trail must show every decision either way), and returns the subset that
// passed plus whether every change passed. A gate.Verify error (as opposed
// to a per-service reject) is a stage failure, matching every other
// pre-apply stage in this cycle: gate infrastructure being unreachable is
// not a policy decision to fail closed on selectively.
//
// Removals are never sent to the gate. Verifying the provenance of an
// image being deleted has no supply-chain value, and doing so anyway
// would let an unverifiable image (deployed before swarmgate, or manually
// labelled managed) permanently block its own removal: per-service mode
// would re-reject it every cycle, and abort-cycle mode would freeze the
// whole cycle on it. Removals pass through untouched and unlogged here.
//
// The passed subset alone is what gate.mode "per-service" applies: a
// reject blocks only that service. "abort-cycle" instead uses allPassed —
// the caller in cycle() applies nothing at all when it's false, regardless
// of how many changes did pass.
func verify(ctx context.Context, g gate.Gate, changes []apply.Change, emit func(telemetry.Event), now func() time.Time) (passed []apply.Change, allPassed bool, err error) {
	var toVerify []apply.Change
	for _, c := range changes {
		if c.Action != apply.ActionRemove {
			toVerify = append(toVerify, c)
		}
	}

	t := now()
	verdicts, err := g.Verify(ctx, toVerify)
	if err != nil {
		return nil, false, err
	}
	// One batched Verify call covers every service; each per-service event
	// carries the whole batch's duration, not an individual share of it.
	verifyDur := ms(now().Sub(t))
	passed = make([]apply.Change, 0, len(changes))
	allPassed = true
	passByIndex := make([]bool, len(verdicts))
	for i, v := range verdicts {
		fields := map[string]any{"image": v.Image, "outcome": "pass"}
		if !v.Pass {
			fields["outcome"] = "reject"
			fields["reason"] = v.Reason
			allPassed = false
		}
		emit(telemetry.Event{Stage: telemetry.StageVerify, Service: v.Service, DurMS: verifyDur, Fields: fields})
		if v.Pass {
			passByIndex[i] = true
		}
	}
	verifyIndex := 0
	passedByChange := make([]bool, len(changes))
	for i, c := range changes {
		if c.Action == apply.ActionRemove {
			if c.Recreate && i+1 < len(changes) && changes[i+1].Recreate && changes[i+1].Action == apply.ActionCreate {
				if verifyIndex < len(passByIndex) && passByIndex[verifyIndex] {
					passedByChange[i] = true
				}
			} else {
				passedByChange[i] = true
			}
			continue
		}
		if verifyIndex < len(passByIndex) && passByIndex[verifyIndex] {
			passedByChange[i] = true
		}
		verifyIndex++
	}
	for i, c := range changes {
		if passedByChange[i] {
			passed = append(passed, c)
		}
	}
	return passed, allPassed, nil
}

func ms(d time.Duration) int64 {
	return d.Milliseconds()
}

func digestOf(image string) string {
	if _, digest, ok := strings.Cut(image, "@"); ok {
		return digest
	}
	return ""
}

// preApplyError marks a cycle failure that happened before any apply was
// attempted — source, parse, resolve, observe, or gate infrastructure.
// run() backs off on this kind specifically: these are the failure modes
// where hammering the same dependency again immediately (a struggling
// git remote, registry, or Docker socket) is the wrong response. Failures
// during or after apply (partial apply failure, unconverged) already
// carry their own "believed" telemetry describing exactly what state was
// reached and retry on the normal poll cadence instead.
type preApplyError struct{ err error }

func (e *preApplyError) Error() string { return e.err.Error() }
func (e *preApplyError) Unwrap() error { return e.err }

// preApplyErr wraps err as a preApplyError and emits the error telemetry
// event every pre-apply stage failure needs — previously only apply and
// convergence failures were recorded at all, leaving the JSONL trail
// silent about why a cycle never got that far.
func preApplyErr(emit func(telemetry.Event), stage string, err error) error {
	emit(telemetry.Event{
		Stage:  telemetry.StageError,
		Fields: map[string]any{"stage": stage, "error": err.Error()},
	})
	return &preApplyError{fmt.Errorf("%s: %w", stage, err)}
}
