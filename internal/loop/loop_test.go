package loop

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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

type captureRecorder struct {
	mu     sync.Mutex
	events []telemetry.Event
}

func (r *captureRecorder) Emit(e telemetry.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *captureRecorder) all() []telemetry.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]telemetry.Event(nil), r.events...)
}

// fakeDropRecorder lets tests set Dropped() directly, unlike
// captureRecorder, which always reports 0.
type fakeDropRecorder struct {
	captureRecorder
	dropped uint64
}

func (r *fakeDropRecorder) Dropped() uint64 { return r.dropped }

func (r *captureRecorder) stages() []string {
	var out []string
	for _, e := range r.all() {
		out = append(out, e.Stage)
	}
	return out
}

type fakeSource struct {
	mu      sync.Mutex
	commit  source.Commit
	files   []source.StackFile
	err     error
	fetches int
	block   chan struct{} // when set, Fetch waits for one receive per call
}

func (s *fakeSource) Fetch(ctx context.Context) (source.Commit, []source.StackFile, error) {
	s.mu.Lock()
	s.fetches++
	block := s.block
	s.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return source.Commit{}, nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commit, s.files, s.err
}

func (s *fakeSource) fetchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches
}

func testSpec(name string) spec.ServiceSpec {
	return spec.Normalize("web", spec.ServiceSpec{
		Name:     name,
		Image:    "registry.local/app:v1",
		Replicas: 1,
	})
}

func pinResolver(ctx context.Context, d *spec.DesiredState) error {
	for name, s := range d.Services {
		if !strings.Contains(s.Image, "@sha256:") {
			s.Image += "@sha256:0000"
			d.Services[name] = s
		}
	}
	return nil
}

func testDeps(src *fakeSource, desired spec.DesiredState, obs *observe.FakeObserver, app *apply.FakeApplier, rec telemetry.Recorder) Deps {
	return Deps{
		Source:    src,
		Parse:     func([]source.StackFile) (spec.DesiredState, error) { return desired, nil },
		Resolve:   pinResolver,
		Observer:  obs,
		Applier:   app,
		Converger: NopConverger{},
		Gate:      gate.NoopGate{},
		Recorder:  rec,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:       time.Now,
	}
}

func testConfig(prune bool) config.Config {
	return config.Config{
		PollInterval:    time.Hour, // tests drive wakes manually
		Prune:           prune,
		ConvergeTimeout: time.Second,
		StageTimeout:    time.Minute, // generous: tests block fakes deliberately, not via real slowness
	}
}

func TestCycleHappyPathEventOrder(t *testing.T) {
	rec := &captureRecorder{}
	src := &fakeSource{commit: source.Commit{SHA: "abc123"}}
	desired := spec.DesiredState{Services: map[string]spec.ServiceSpec{
		"web_a": testSpec("web_a"),
	}}
	obs := &observe.FakeObserver{}
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{}}, nil)
	app := &apply.FakeApplier{}

	if err := RunOnce(context.Background(), testDeps(src, desired, obs, app, rec), testConfig(true)); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	want := []string{"poll", "parse", "resolve", "observe", "diff", "verify", "apply", "converged"}
	got := rec.stages()
	if len(got) != len(want) {
		t.Fatalf("stages = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stage[%d] = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}

	events := rec.all()
	runID := events[0].RunID
	if runID == "" {
		t.Fatal("empty run_id")
	}
	for _, e := range events {
		if e.RunID != runID {
			t.Fatalf("run_id not constant: %q vs %q", e.RunID, runID)
		}
	}
	if events[0].Fields["commit"] != "abc123" {
		t.Fatalf("poll commit = %v", events[0].Fields)
	}
	if events[2].Service != "web_a" || events[2].Fields["digest"] != "sha256:0000" {
		t.Fatalf("resolve event = %+v", events[2])
	}
	if events[5].Service != "web_a" || events[5].Fields["outcome"] != "pass" {
		t.Fatalf("verify event = %+v", events[5])
	}
	if events[6].Fields["action"] != "create" || events[6].Fields["outcome"] != "ok" {
		t.Fatalf("apply event = %+v", events[6])
	}
	last := events[len(events)-1]
	if last.Fields["commit"] != "abc123" || last.Fields["services"] != 1 {
		t.Fatalf("converged event = %+v", last)
	}
	if applied := app.Applied(); len(applied) != 1 || applied[0].Action != apply.ActionCreate {
		t.Fatalf("applied = %+v", applied)
	}
}

func TestCycleApplyErrorIsolationRecordsBelievedState(t *testing.T) {
	rec := &captureRecorder{}
	src := &fakeSource{commit: source.Commit{SHA: "abc123"}}
	desired := spec.DesiredState{Services: map[string]spec.ServiceSpec{
		"web_a": testSpec("web_a"),
		"web_b": testSpec("web_b"),
	}}
	obs := &observe.FakeObserver{}
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{}}, nil)
	app := &apply.FakeApplier{}
	app.Fail("web_a", errors.New("boom"))

	err := RunOnce(context.Background(), testDeps(src, desired, obs, app, rec), testConfig(true))
	if err == nil {
		t.Fatal("RunOnce must fail when any apply fails")
	}

	var applyOutcomes []string
	var errorEvents []telemetry.Event
	convergedSeen := false
	for _, e := range rec.all() {
		switch e.Stage {
		case telemetry.StageApply:
			applyOutcomes = append(applyOutcomes, e.Service+"="+e.Fields["outcome"].(string))
		case telemetry.StageError:
			errorEvents = append(errorEvents, e)
		case telemetry.StageConverged:
			convergedSeen = true
		}
	}
	if len(applyOutcomes) != 2 || applyOutcomes[0] != "web_a=error" || applyOutcomes[1] != "web_b=ok" {
		t.Fatalf("apply outcomes = %v", applyOutcomes)
	}
	if convergedSeen {
		t.Fatal("converged must not be emitted after apply failures")
	}
	if len(errorEvents) != 1 {
		t.Fatalf("error events = %+v", errorEvents)
	}
	f := errorEvents[0].Fields
	if f["believed"] != "partial" {
		t.Fatalf("believed = %v", f["believed"])
	}
	if applied, _ := f["applied"].([]string); len(applied) != 1 || applied[0] != "web_b" {
		t.Fatalf("applied = %v", f["applied"])
	}
	if pending, _ := f["pending"].([]string); len(pending) != 1 || pending[0] != "web_a" {
		t.Fatalf("pending = %v", f["pending"])
	}
}

func TestPruneGatesRemoveApplication(t *testing.T) {
	// A first-ever cycle never attributes drift (no prior commit to compare
	// against), so this only exercises plan()'s prune gate: apply the
	// removal when pruning, skip it otherwise.
	for _, prune := range []bool{false, true} {
		rec := &captureRecorder{}
		src := &fakeSource{commit: source.Commit{SHA: "abc123"}}
		desired := spec.DesiredState{Services: map[string]spec.ServiceSpec{}}
		obs := &observe.FakeObserver{}
		obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{
			"web_gone": testSpec("web_gone"),
		}}, nil)
		app := &apply.FakeApplier{}

		if err := RunOnce(context.Background(), testDeps(src, desired, obs, app, rec), testConfig(prune)); err != nil {
			t.Fatalf("prune=%v RunOnce: %v", prune, err)
		}

		removed := len(app.Applied()) == 1
		if prune && !removed {
			t.Fatalf("prune=true: service was not removed")
		}
		if !prune && removed {
			t.Fatalf("prune=false: service was removed despite prune being off")
		}
		for _, e := range rec.all() {
			if e.Stage == telemetry.StageDrift {
				t.Fatalf("first cycle must not attribute drift: %+v", e)
			}
		}
	}
}

// runTwoCycles drives cycle() twice against shared drift-attribution state
// with the given commit both times, so the second cycle sees an unchanged
// commit and can attribute drift. The warmup cycle gets its own throwaway
// recorder and applier: it exists solely to establish "previous cycle"
// state, and its apply of the very same diff (unrelated to drift gating —
// plan() applies regardless of commit-sameness) must not be mistaken for
// the second, measured cycle's behavior. Returns the second cycle's
// recorder and the applier used for it.
func runTwoCycles(t *testing.T, prune bool, sha string, obs *observe.FakeObserver, seedHints func(st *cycleState)) (*captureRecorder, *apply.FakeApplier) {
	t.Helper()
	st := newCycleState()
	src := &fakeSource{commit: source.Commit{SHA: sha}}
	desired := spec.DesiredState{Services: map[string]spec.ServiceSpec{}}
	warmup := &captureRecorder{}
	warmupApp := &apply.FakeApplier{}
	deps := testDeps(src, desired, obs, warmupApp, warmup)
	if err := cycle(context.Background(), deps, testConfig(prune), st, telemetry.NewRunID()); err != nil {
		t.Fatalf("warmup cycle: %v", err)
	}
	if seedHints != nil {
		seedHints(st)
	}
	rec := &captureRecorder{}
	app := &apply.FakeApplier{}
	deps.Recorder = rec
	deps.Applier = app
	if err := cycle(context.Background(), deps, testConfig(prune), st, telemetry.NewRunID()); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	return rec, app
}

func driftEvents(rec *captureRecorder) []telemetry.Event {
	var out []telemetry.Event
	for _, e := range rec.all() {
		if e.Stage == telemetry.StageDrift {
			out = append(out, e)
		}
	}
	return out
}

func TestDriftAttributionUnmanagedRemoval(t *testing.T) {
	for _, prune := range []bool{false, true} {
		obs := &observe.FakeObserver{}
		obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{
			"web_gone": testSpec("web_gone"),
		}}, nil)

		rec, app := runTwoCycles(t, prune, "abc123", obs, nil)

		drifts := driftEvents(rec)
		if len(drifts) != 1 {
			t.Fatalf("prune=%v: drift events = %+v, want exactly 1", prune, drifts)
		}
		d := drifts[0]
		if d.Service != "web_gone" || d.Fields["kind"] != "unmanaged" || d.Fields["origin"] != "poll" {
			t.Fatalf("prune=%v: drift event = %+v", prune, d)
		}
		removed := len(app.Applied()) == 1
		if prune && !removed {
			t.Fatalf("prune=true: unchanged-commit removal was not applied")
		}
		if !prune && removed {
			t.Fatalf("prune=false: removal applied despite prune being off")
		}
	}
}

func TestDriftAttributionCreateIsKindRemoved(t *testing.T) {
	// The observer never sees this service at all: the operator deleted it
	// out-of-band while the desired commit stayed the same.
	obs := &observe.FakeObserver{}
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{}}, nil)
	app := &apply.FakeApplier{}

	// runTwoCycles seeds desired as empty; build desired state directly
	// instead so both cycles want "web_a".
	st := newCycleState()
	src := &fakeSource{commit: source.Commit{SHA: "abc123"}}
	desired := spec.DesiredState{Services: map[string]spec.ServiceSpec{"web_a": testSpec("web_a")}}
	warmup := &captureRecorder{}
	deps := testDeps(src, desired, obs, app, warmup)
	if err := cycle(context.Background(), deps, testConfig(true), st, telemetry.NewRunID()); err != nil {
		t.Fatalf("warmup cycle: %v", err)
	}
	rec := &captureRecorder{}
	deps.Recorder = rec
	if err := cycle(context.Background(), deps, testConfig(true), st, telemetry.NewRunID()); err != nil {
		t.Fatalf("second cycle: %v", err)
	}

	drifts := driftEvents(rec)
	if len(drifts) != 1 || drifts[0].Service != "web_a" || drifts[0].Fields["kind"] != "removed" {
		t.Fatalf("drift events = %+v, want one kind=removed for web_a", drifts)
	}
}

func TestDriftAttributionUpdateKindFromChangedFields(t *testing.T) {
	// Covers the two-cycle wiring end to end; driftKindForUpdate's full
	// priority table (including the env case, and fields with no
	// representable drift kind) is unit-tested directly below since those
	// permutations don't need a live diff to exercise.
	tests := []struct {
		name       string
		mutateHave func(spec.ServiceSpec) spec.ServiceSpec
		wantKind   string
	}{
		{"image", func(s spec.ServiceSpec) spec.ServiceSpec { s.Image = "registry.local/app:old"; return s }, "image"},
		{"replicas", func(s spec.ServiceSpec) spec.ServiceSpec { s.Replicas = 9; return s }, "replicas"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := testSpec("web_a")
			want.Image += "@sha256:0000"
			have := tt.mutateHave(want)

			obs := &observe.FakeObserver{}
			obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{"web_a": have}}, nil)

			st := newCycleState()
			src := &fakeSource{commit: source.Commit{SHA: "abc123"}}
			desired := spec.DesiredState{Services: map[string]spec.ServiceSpec{"web_a": want}}
			warmup := &captureRecorder{}
			deps := testDeps(src, desired, obs, &apply.FakeApplier{}, warmup)
			if err := cycle(context.Background(), deps, testConfig(true), st, telemetry.NewRunID()); err != nil {
				t.Fatalf("warmup cycle: %v", err)
			}
			rec := &captureRecorder{}
			deps.Recorder = rec
			deps.Applier = &apply.FakeApplier{}
			if err := cycle(context.Background(), deps, testConfig(true), st, telemetry.NewRunID()); err != nil {
				t.Fatalf("second cycle: %v", err)
			}

			drifts := driftEvents(rec)
			if len(drifts) != 1 || drifts[0].Fields["kind"] != tt.wantKind {
				t.Fatalf("drift events = %+v, want one kind=%s", drifts, tt.wantKind)
			}
		})
	}
}

func TestDriftKindForUpdate(t *testing.T) {
	tests := []struct {
		name     string
		changed  []string
		wantKind string
		wantOK   bool
	}{
		{"image alone", []string{"image"}, "image", true},
		{"replicas alone", []string{"replicas"}, "replicas", true},
		{"env alone", []string{"env"}, "env", true},
		// diff.Changed is always produced in this fixed relative order
		// (image, replicas, env, labels, networks, ports, healthcheck);
		// driftKindForUpdate takes the first representable field it sees,
		// which is equivalent to that fixed priority in real usage.
		{"image wins over replicas and env", []string{"image", "replicas", "env"}, "image", true},
		{"replicas wins over env and labels", []string{"replicas", "env", "labels"}, "replicas", true},
		{"labels only has no representable kind", []string{"labels"}, "", false},
		{"networks/ports/healthcheck only has no representable kind", []string{"networks", "ports", "healthcheck"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, ok := driftKindForUpdate(tt.changed)
			if ok != tt.wantOK || kind != tt.wantKind {
				t.Fatalf("driftKindForUpdate(%v) = (%q, %v), want (%q, %v)", tt.changed, kind, ok, tt.wantKind, tt.wantOK)
			}
		})
	}
}

func TestDriftAttributionOriginEventVsPoll(t *testing.T) {
	obs := &observe.FakeObserver{}
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{
		"web_gone": testSpec("web_gone"),
	}}, nil)

	rec, _ := runTwoCycles(t, true, "abc123", obs, func(st *cycleState) {
		st.recordHint("web_gone", time.Now())
	})

	drifts := driftEvents(rec)
	if len(drifts) != 1 || drifts[0].Fields["origin"] != "event" {
		t.Fatalf("drift events = %+v, want origin=event when a hint was recorded", drifts)
	}
}

func TestDriftAttributionNoDriftOnCommitChange(t *testing.T) {
	// Second cycle's commit differs from the first: the removal this time
	// is a genuine git-driven deployment, not drift, so no event fires.
	obs := &observe.FakeObserver{}
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{
		"web_gone": testSpec("web_gone"),
	}}, nil)
	app := &apply.FakeApplier{}

	st := newCycleState()
	src := &fakeSource{commit: source.Commit{SHA: "commit-1"}}
	desired := spec.DesiredState{Services: map[string]spec.ServiceSpec{}}
	warmup := &captureRecorder{}
	deps := testDeps(src, desired, obs, app, warmup)
	if err := cycle(context.Background(), deps, testConfig(true), st, telemetry.NewRunID()); err != nil {
		t.Fatalf("warmup cycle: %v", err)
	}
	src.commit = source.Commit{SHA: "commit-2"}
	rec := &captureRecorder{}
	deps.Recorder = rec
	if err := cycle(context.Background(), deps, testConfig(true), st, telemetry.NewRunID()); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if drifts := driftEvents(rec); len(drifts) != 0 {
		t.Fatalf("commit changed: drift events = %+v, want none", drifts)
	}
}

func TestWatchEventsRecordsHintAndWakes(t *testing.T) {
	hints := make(chan observe.DriftHint, 1)
	hints <- observe.DriftHint{Service: "web_a", At: time.Now()}
	src := EventSource(func(ctx context.Context) <-chan observe.DriftHint {
		out := make(chan observe.DriftHint)
		go func() {
			defer close(out)
			select {
			case h := <-hints:
				select {
				case out <- h:
				case <-ctx.Done():
				}
			case <-ctx.Done():
			}
		}()
		return out
	})

	st := newCycleState()
	wake := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go watchEvents(ctx, src, st, wake, log, nil)

	select {
	case <-wake:
	case <-time.After(time.Second):
		t.Fatal("watchEvents did not wake on hint")
	}
	if !st.takeHint("web_a", time.Time{}) {
		t.Fatal("watchEvents did not record the hint")
	}
}

// TestWatchEventsReconnectsAfterPanic is a regression test: a panic in
// watchEvents' own bookkeeping used to exit the goroutine for good,
// permanently disabling event-driven wake for the rest of the process's
// life. The first src(ctx) call returns a channel that panics as soon as
// it is ranged over; the second call returns a real hint. Only a
// reconnect proves the loop survives the first panic.
func TestWatchEventsReconnectsAfterPanic(t *testing.T) {
	// src is called directly inside watchEventsOnce's own goroutine (the
	// one its recover() guards), so panicking here — rather than in a
	// separate producer goroutine recover() can never see — exercises the
	// real code path.
	var calls int32
	src := EventSource(func(ctx context.Context) <-chan observe.DriftHint {
		if atomic.AddInt32(&calls, 1) == 1 {
			panic("boom: simulated bookkeeping panic")
		}
		out := make(chan observe.DriftHint)
		go func() {
			defer close(out)
			select {
			case out <- observe.DriftHint{Service: "web_a", At: time.Now()}:
			case <-ctx.Done():
			}
		}()
		return out
	})

	st := newCycleState()
	wake := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	instant := func(time.Duration) <-chan time.Time {
		c := make(chan time.Time, 1)
		c <- time.Now()
		return c
	}
	go watchEvents(ctx, src, st, wake, log, instant)

	select {
	case <-wake:
	case <-time.After(time.Second):
		t.Fatal("watchEvents did not reconnect and wake after the panic")
	}
	if !st.takeHint("web_a", time.Time{}) {
		t.Fatal("watchEvents did not record the hint from the reconnected subscription")
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("src called %d times, want at least 2 (the panicking attempt plus the reconnect)", got)
	}
}

// TestDriftAttributionStaleHintFallsBackToPoll is a regression test: an
// unconsumed hint must expire rather than mislabel a later, unrelated
// drift as event-driven.
func TestDriftAttributionStaleHintFallsBackToPoll(t *testing.T) {
	obs := &observe.FakeObserver{}
	// Cycle 1: cluster matches desired, empty diff, hint not consumed.
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{}}, nil)

	st := newCycleState()
	// Stale hint, recorded before any cycle has run.
	st.recordHint("web_gone", time.Now())

	src := &fakeSource{commit: source.Commit{SHA: "abc123"}}
	desired := spec.DesiredState{Services: map[string]spec.ServiceSpec{}}
	warmup := &captureRecorder{}
	deps := testDeps(src, desired, obs, &apply.FakeApplier{}, warmup)
	if err := cycle(context.Background(), deps, testConfig(true), st, telemetry.NewRunID()); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}

	// Cycle 2: drift appears (unmanaged leftover), detected by poll.
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{
		"web_gone": testSpec("web_gone"),
	}}, nil)
	rec := &captureRecorder{}
	deps.Recorder = rec
	if err := cycle(context.Background(), deps, testConfig(true), st, telemetry.NewRunID()); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}

	drifts := driftEvents(rec)
	if len(drifts) != 1 {
		t.Fatalf("drift events = %+v, want exactly 1", drifts)
	}
	if drifts[0].Fields["origin"] != "poll" {
		t.Fatalf("origin = %v, want poll — the pre-cycle-1 hint is stale and must not attribute this drift", drifts[0].Fields["origin"])
	}
}

func TestCycleEmptyDiffEmitsStageEventsOnly(t *testing.T) {
	rec := &captureRecorder{}
	src := &fakeSource{commit: source.Commit{SHA: "abc123"}}
	pinned := testSpec("web_a")
	pinned.Image += "@sha256:0000"
	desired := spec.DesiredState{Services: map[string]spec.ServiceSpec{"web_a": pinned}}
	obs := &observe.FakeObserver{}
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{"web_a": pinned}}, nil)
	app := &apply.FakeApplier{}

	if err := RunOnce(context.Background(), testDeps(src, desired, obs, app, rec), testConfig(true)); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got := rec.stages()
	want := []string{"poll", "parse", "resolve", "observe", "diff"}
	if len(got) != len(want) {
		t.Fatalf("stages = %v, want %v", got, want)
	}
	if len(app.Applied()) != 0 {
		t.Fatalf("nothing must be applied on empty diff")
	}
}

type recordingConverger struct {
	mu    sync.Mutex
	diffs []diff.Diff
}

func (c *recordingConverger) AwaitConverged(_ context.Context, d diff.Diff, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.diffs = append(c.diffs, d)
	return nil
}

func TestCycleAwaitsOnlyAppliedChanges(t *testing.T) {
	for _, prune := range []bool{false, true} {
		rec := &captureRecorder{}
		src := &fakeSource{commit: source.Commit{SHA: "abc123"}}
		desired := spec.DesiredState{Services: map[string]spec.ServiceSpec{
			"web_a": testSpec("web_a"),
		}}
		obs := &observe.FakeObserver{}
		obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{
			"web_gone": testSpec("web_gone"),
		}}, nil)
		app := &apply.FakeApplier{}
		conv := &recordingConverger{}

		deps := testDeps(src, desired, obs, app, rec)
		deps.Converger = conv
		if err := RunOnce(context.Background(), deps, testConfig(prune)); err != nil {
			t.Fatalf("prune=%v RunOnce: %v", prune, err)
		}
		if len(conv.diffs) != 1 {
			t.Fatalf("prune=%v converger calls = %d", prune, len(conv.diffs))
		}
		removes := len(conv.diffs[0].Removes)
		if prune && removes != 1 {
			t.Fatalf("prune=true must await the removal, got %d", removes)
		}
		if !prune && removes != 0 {
			t.Fatalf("prune=false must not await skipped removals, got %d", removes)
		}
	}
}

func TestRunCoalescesWakes(t *testing.T) {
	rec := &captureRecorder{}
	release := make(chan struct{})
	src := &fakeSource{commit: source.Commit{SHA: "abc123"}, block: release}
	desired := spec.DesiredState{Services: map[string]spec.ServiceSpec{}}
	obs := &observe.FakeObserver{}
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{}}, nil)
	app := &apply.FakeApplier{}

	deps := testDeps(src, desired, obs, app, rec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wake := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, deps, testConfig(true), wake, newCycleState()) }()

	// Multiple wakes arriving while cycle 1 is still in flight must
	// coalesce into exactly one immediate rerun.
	requestWake(wake)
	requestWake(wake)
	requestWake(wake)
	release <- struct{}{} // cycle 1 completes; coalesced wake triggers cycle 2
	release <- struct{}{} // cycle 2 completes

	// No further wakes: fetch count must settle at exactly 2.
	deadline := time.After(2 * time.Second)
	for src.fetchCount() < 2 {
		select {
		case <-deadline:
			t.Fatalf("expected 2 fetches, got %d", src.fetchCount())
		case <-time.After(10 * time.Millisecond):
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := src.fetchCount(); n != 2 {
		t.Fatalf("wakes not coalesced: %d fetches", n)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run returned %v", err)
	}
}

// fakeGate returns a scripted verdict per service name; unlisted services
// pass by default.
type fakeGate struct {
	rejects map[string]string // service -> reason
	err     error
}

func (g *fakeGate) Verify(_ context.Context, changes []apply.Change) ([]gate.Verdict, error) {
	if g.err != nil {
		return nil, g.err
	}
	verdicts := make([]gate.Verdict, len(changes))
	for i, c := range changes {
		v := gate.Verdict{Service: c.Spec.Name, Image: c.Spec.Image, Pass: true}
		if reason, reject := g.rejects[c.Spec.Name]; reject {
			v.Pass, v.Reason = false, reason
		}
		verdicts[i] = v
	}
	return verdicts, nil
}

func TestVerifyFiltersRejectedChangesAndEmitsEvents(t *testing.T) {
	changes := []apply.Change{
		{Action: apply.ActionCreate, Spec: testSpec("web_a")},
		{Action: apply.ActionCreate, Spec: testSpec("web_b")},
	}
	g := &fakeGate{rejects: map[string]string{"web_b": "unsigned"}}
	var events []telemetry.Event
	passed, allPassed, err := verify(context.Background(), g, changes, func(e telemetry.Event) { events = append(events, e) }, time.Now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if allPassed {
		t.Fatal("allPassed = true, want false with a rejected change present")
	}
	if len(passed) != 1 || passed[0].Spec.Name != "web_a" {
		t.Fatalf("passed = %+v, want only web_a", passed)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v, want 2", events)
	}
	if events[0].Service != "web_a" || events[0].Fields["outcome"] != "pass" {
		t.Fatalf("web_a event = %+v", events[0])
	}
	if events[1].Service != "web_b" || events[1].Fields["outcome"] != "reject" || events[1].Fields["reason"] != "unsigned" {
		t.Fatalf("web_b event = %+v", events[1])
	}
}

// TestVerifySkipsGateForRemovals is a regression test: verifying the
// provenance of an image being deleted has no supply-chain value, and an
// unverifiable image would otherwise block its own removal forever. The
// removal targets a service name fakeGate is configured to reject, so if
// it reached the gate at all it would come back rejected; instead it must
// pass through untouched with no verify event.
// TestCycleResolveAndVerifyEventsCarryDuration is a regression test:
// resolve and verify previously emitted no DurMS, leaving push-to-
// converged latency undecomposable past poll/parse/observe/apply. Both
// stages run one batched call per cycle, so every per-service event
// carries the whole batch's duration.
func TestCycleResolveAndVerifyEventsCarryDuration(t *testing.T) {
	rec := &captureRecorder{}
	src := &fakeSource{commit: source.Commit{SHA: "abc123"}}
	desired := spec.DesiredState{Services: map[string]spec.ServiceSpec{
		"web_a": testSpec("web_a"),
	}}
	obs := &observe.FakeObserver{}
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{}}, nil)
	app := &apply.FakeApplier{}

	deps := testDeps(src, desired, obs, app, rec)
	deps.Resolve = func(ctx context.Context, d *spec.DesiredState) error {
		time.Sleep(5 * time.Millisecond)
		return pinResolver(ctx, d)
	}

	if err := RunOnce(context.Background(), deps, testConfig(true)); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	var sawResolve, sawVerify bool
	for _, e := range rec.all() {
		switch e.Stage {
		case telemetry.StageResolve:
			sawResolve = true
			if e.DurMS <= 0 {
				t.Errorf("resolve event DurMS = %d, want > 0", e.DurMS)
			}
		case telemetry.StageVerify:
			sawVerify = true
			if e.DurMS < 0 {
				t.Errorf("verify event DurMS = %d, want >= 0", e.DurMS)
			}
		}
	}
	if !sawResolve || !sawVerify {
		t.Fatalf("sawResolve=%v sawVerify=%v, want both", sawResolve, sawVerify)
	}
}

// TestWarnDroppedRateLimiting is a regression test: warnDropped used to
// re-warn on every cycle forever once any event dropped, since Dropped()
// is cumulative and the check was a plain > 0. It must warn once when
// drops first appear and again only when the count rises further.
func TestWarnDroppedRateLimiting(t *testing.T) {
	var buf strings.Builder
	deps := Deps{
		Log:      slog.New(slog.NewTextHandler(&buf, nil)),
		Recorder: &fakeDropRecorder{},
	}
	rec := deps.Recorder.(*fakeDropRecorder)
	var lastWarned uint64
	count := func() int { return strings.Count(buf.String(), "telemetry events dropped") }

	warnDropped(deps, &lastWarned)
	if count() != 0 {
		t.Fatalf("warnings with 0 drops = %d, want 0", count())
	}

	rec.dropped = 1
	warnDropped(deps, &lastWarned)
	if count() != 1 {
		t.Fatalf("warnings after first drop = %d, want 1", count())
	}

	warnDropped(deps, &lastWarned)
	warnDropped(deps, &lastWarned)
	if count() != 1 {
		t.Fatalf("warnings after unchanged count = %d, want still 1", count())
	}

	rec.dropped = 3
	warnDropped(deps, &lastWarned)
	if count() != 2 {
		t.Fatalf("warnings after count rose = %d, want 2", count())
	}
}

func TestVerifySkipsGateForRemovals(t *testing.T) {
	changes := []apply.Change{
		{Action: apply.ActionCreate, Spec: testSpec("web_a")},
		{Action: apply.ActionRemove, Spec: testSpec("web_stale")},
	}
	g := &fakeGate{rejects: map[string]string{"web_stale": "unsigned"}}
	var events []telemetry.Event
	passed, allPassed, err := verify(context.Background(), g, changes, func(e telemetry.Event) { events = append(events, e) }, time.Now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !allPassed {
		t.Fatal("allPassed = false, want true")
	}
	if len(passed) != 2 {
		t.Fatalf("passed = %+v, want both changes", passed)
	}
	for _, e := range events {
		if e.Service == "web_stale" {
			t.Fatalf("unexpected verify event for a removal: %+v", e)
		}
	}
}

func TestVerifyPropagatesGateError(t *testing.T) {
	changes := []apply.Change{{Action: apply.ActionCreate, Spec: testSpec("web_a")}}
	g := &fakeGate{err: errors.New("registry unreachable")}
	_, _, err := verify(context.Background(), g, changes, func(telemetry.Event) {}, time.Now)
	if err == nil {
		t.Fatal("verify() = nil error, want the gate's error propagated")
	}
}

// threeServiceCycleDeps builds a cycle with 3 create changes and a gate
// that rejects one of them, shared by both gate.mode tests below so the
// only variable between them is cfg.Gate.Mode.
func threeServiceCycleDeps(rejectService string) (Deps, *apply.FakeApplier, *captureRecorder) {
	rec := &captureRecorder{}
	src := &fakeSource{commit: source.Commit{SHA: "abc123"}}
	desired := spec.DesiredState{Services: map[string]spec.ServiceSpec{
		"web_a": testSpec("web_a"),
		"web_b": testSpec("web_b"),
		"web_c": testSpec("web_c"),
	}}
	obs := &observe.FakeObserver{}
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{}}, nil)
	app := &apply.FakeApplier{}

	deps := testDeps(src, desired, obs, app, rec)
	deps.Gate = &fakeGate{rejects: map[string]string{rejectService: "unsigned"}}
	return deps, app, rec
}

func TestCycleGateModePerServiceAppliesPassingChanges(t *testing.T) {
	deps, app, rec := threeServiceCycleDeps("web_b")
	cfg := testConfig(true)
	cfg.Gate.Mode = config.GateModePerService

	if err := RunOnce(context.Background(), deps, cfg); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	applied := app.Applied()
	if len(applied) != 2 {
		t.Fatalf("applied = %+v, want 2 (web_a and web_c only)", applied)
	}
	for _, c := range applied {
		if c.Spec.Name == "web_b" {
			t.Fatalf("rejected service web_b was applied: %+v", applied)
		}
	}
	for _, e := range rec.all() {
		if e.Stage == telemetry.StageError {
			t.Fatalf("unexpected error event in per-service mode: %+v", e)
		}
	}
}

// TestCycleGateRejectDoesNotBlockConvergenceWait is a regression test:
// AwaitConverged must be called with only the changes the gate actually
// let through, not the raw pre-gate diff. Before this fix, a per-service
// gate reject still left the rejected service in the diff handed to
// AwaitConverged, which would wait the full timeout for a service that
// was never applied and never will be — defeating "a reject blocks only
// that service" in practice, since it forced the whole cycle to eat a
// convergence timeout on every gate rejection.
func TestCycleGateRejectDoesNotBlockConvergenceWait(t *testing.T) {
	deps, _, _ := threeServiceCycleDeps("web_b")
	conv := &recordingConverger{}
	deps.Converger = conv
	cfg := testConfig(true)
	cfg.Gate.Mode = config.GateModePerService

	if err := RunOnce(context.Background(), deps, cfg); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(conv.diffs) != 1 {
		t.Fatalf("converger calls = %d, want 1", len(conv.diffs))
	}
	for _, s := range conv.diffs[0].Creates {
		if s.Name == "web_b" {
			t.Fatal("gate-rejected web_b was included in the convergence wait")
		}
	}
	if len(conv.diffs[0].Creates) != 2 {
		t.Fatalf("awaited creates = %+v, want web_a and web_c only", conv.diffs[0].Creates)
	}
}

// TestCycleAllRejectedEmitsNoConverged is a regression test: a cycle whose
// changes are all gate-rejected applies nothing and must not emit
// converged, even though the pre-filter diff was non-empty.
func TestCycleAllRejectedEmitsNoConverged(t *testing.T) {
	rec := &captureRecorder{}
	src := &fakeSource{commit: source.Commit{SHA: "abc123"}}
	desired := spec.DesiredState{Services: map[string]spec.ServiceSpec{
		"web_a": testSpec("web_a"),
	}}
	obs := &observe.FakeObserver{}
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{}}, nil)
	app := &apply.FakeApplier{}

	deps := testDeps(src, desired, obs, app, rec)
	deps.Gate = &fakeGate{rejects: map[string]string{"web_a": "unsigned"}}
	cfg := testConfig(true)
	cfg.Gate.Mode = config.GateModePerService

	if err := RunOnce(context.Background(), deps, cfg); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if applied := app.Applied(); len(applied) != 0 {
		t.Fatalf("applied = %+v, want none", applied)
	}
	for _, e := range rec.all() {
		if e.Stage == telemetry.StageConverged {
			t.Fatalf("converged event emitted for a cycle that applied nothing: %+v", e)
		}
	}
}

// TestCycleSkippedRemovesEmitNoConverged is the prune-off variant: a
// removes-only diff with pruning disabled changes nothing, so no
// converged event may fire.
func TestCycleSkippedRemovesEmitNoConverged(t *testing.T) {
	rec := &captureRecorder{}
	src := &fakeSource{commit: source.Commit{SHA: "abc123"}}
	obs := &observe.FakeObserver{}
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{
		"web_stale": testSpec("web_stale"),
	}}, nil)
	app := &apply.FakeApplier{}

	deps := testDeps(src, spec.DesiredState{Services: map[string]spec.ServiceSpec{}}, obs, app, rec)

	if err := RunOnce(context.Background(), deps, testConfig(false)); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	for _, e := range rec.all() {
		if e.Stage == telemetry.StageConverged {
			t.Fatalf("converged event emitted for a cycle that skipped its only change: %+v", e)
		}
	}
}

func TestCycleGateModeAbortCycleAppliesNothing(t *testing.T) {
	deps, app, rec := threeServiceCycleDeps("web_b")
	cfg := testConfig(true)
	cfg.Gate.Mode = config.GateModeAbortCycle

	err := RunOnce(context.Background(), deps, cfg)
	if err == nil {
		t.Fatal("RunOnce must fail when abort-cycle mode aborts")
	}

	if applied := app.Applied(); len(applied) != 0 {
		t.Fatalf("applied = %+v, want none — abort-cycle must apply nothing", applied)
	}

	var verifyEvents, applyEvents int
	var abortEvent *telemetry.Event
	for _, e := range rec.all() {
		switch e.Stage {
		case telemetry.StageVerify:
			verifyEvents++
		case telemetry.StageApply:
			applyEvents++
		case telemetry.StageConverged:
			t.Fatal("converged must not be emitted when abort-cycle aborts")
		case telemetry.StageError:
			abortEvent = &e
		}
	}
	if verifyEvents != 3 {
		t.Fatalf("verify events = %d, want 3 (one per change, pass and reject alike)", verifyEvents)
	}
	if applyEvents != 0 {
		t.Fatalf("apply events = %d, want 0", applyEvents)
	}
	if abortEvent == nil {
		t.Fatal("no error event emitted")
	}
	if abortEvent.Fields["believed"] != "aborted" || abortEvent.Fields["reason"] != "gate" {
		t.Fatalf("error event fields = %+v, want believed=aborted reason=gate", abortEvent.Fields)
	}
}

// flakySource fails its first `failures` Fetch calls with a transient
// error, then succeeds; each call signals on `called` so a test can
// synchronize without polling or sleeping.
type flakySource struct {
	mu       sync.Mutex
	failures int
	calls    int
	commit   source.Commit
	called   chan struct{}
}

func (s *flakySource) Fetch(context.Context) (source.Commit, []source.StackFile, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	if s.called != nil {
		s.called <- struct{}{}
	}
	if n <= s.failures {
		return source.Commit{}, nil, errors.New("transient source error")
	}
	return s.commit, nil, nil
}

// TestRunBacksOffOnPreApplyFailureAndResetsOnSuccess drives run() (not
// RunOnce, which never loops) against a source that fails 3 times before
// succeeding, with a fake After that records requested delays instead of
// sleeping. Asserts: one backoff wait per pre-apply failure, each roughly
// double the last (within jitter bounds), and one poll-stage error event
// per failure — the earlier telemetry gap this commit also closes.
func TestRunBacksOffOnPreApplyFailureAndResetsOnSuccess(t *testing.T) {
	rec := &captureRecorder{}
	src := &flakySource{failures: 3, commit: source.Commit{SHA: "abc123"}, called: make(chan struct{}, 10)}
	desired := spec.DesiredState{Services: map[string]spec.ServiceSpec{}}
	obs := &observe.FakeObserver{}
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{}}, nil)
	app := &apply.FakeApplier{}

	var mu sync.Mutex
	var afterCalls []time.Duration
	fakeAfter := func(d time.Duration) <-chan time.Time {
		mu.Lock()
		afterCalls = append(afterCalls, d)
		mu.Unlock()
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	deps := Deps{
		Source:    src,
		Parse:     func([]source.StackFile) (spec.DesiredState, error) { return desired, nil },
		Resolve:   pinResolver,
		Observer:  obs,
		Applier:   app,
		Converger: NopConverger{},
		Gate:      gate.NoopGate{},
		Recorder:  rec,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:       time.Now,
		After:     fakeAfter,
	}

	ctx, cancel := context.WithCancel(context.Background())
	wake := make(chan struct{}, 1)
	st := newCycleState()

	done := make(chan error, 1)
	go func() { done <- run(ctx, deps, testConfig(true), wake, st) }()

	for i := 0; i < 4; i++ {
		select {
		case <-src.called:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for Fetch call %d", i+1)
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run() = %v, want context.Canceled", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(afterCalls) != 3 {
		t.Fatalf("After called %d times, want 3 (once per pre-apply failure)", len(afterCalls))
	}
	if afterCalls[0] < 800*time.Millisecond || afterCalls[0] > 1200*time.Millisecond {
		t.Errorf("afterCalls[0] = %v, want ~1s (base delay ± jitter)", afterCalls[0])
	}
	if afterCalls[1] <= afterCalls[0] {
		t.Errorf("afterCalls[1] = %v, want > afterCalls[0] = %v (exponential growth)", afterCalls[1], afterCalls[0])
	}
	if afterCalls[2] <= afterCalls[1] {
		t.Errorf("afterCalls[2] = %v, want > afterCalls[1] = %v (exponential growth)", afterCalls[2], afterCalls[1])
	}

	var pollErrors int
	for _, e := range rec.all() {
		if e.Stage == telemetry.StageError && e.Fields["stage"] == "poll" {
			pollErrors++
		}
	}
	if pollErrors != 3 {
		t.Fatalf("poll-stage error events = %d, want 3", pollErrors)
	}
}

// TestSafeCycleRecoversPanic is the production-hardening regression test:
// a panic anywhere inside one cycle (a malformed stack file, an
// unexpected SDK response, anything unanticipated) must not crash the
// whole long-running daemon and stop reconciling every other stack it
// manages. If recovery didn't work, this test process itself would crash
// rather than report a failure.
func TestSafeCycleRecoversPanic(t *testing.T) {
	rec := &captureRecorder{}
	src := &fakeSource{commit: source.Commit{SHA: "abc123"}}
	obs := &observe.FakeObserver{}
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{}}, nil)
	app := &apply.FakeApplier{}

	deps := testDeps(src, spec.DesiredState{}, obs, app, rec)
	deps.Parse = func([]source.StackFile) (spec.DesiredState, error) {
		panic("boom: simulated malformed stack file")
	}

	err := RunOnce(context.Background(), deps, testConfig(true))
	if err == nil {
		t.Fatal("RunOnce = nil error, want the recovered panic surfaced as an error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want it to mention the panic value", err)
	}
	var pae *preApplyError
	if !errors.As(err, &pae) {
		t.Error("recovered panic should classify as a preApplyError, so run() backs off on it like any other pre-apply failure")
	}

	var panicEvents int
	for _, e := range rec.all() {
		if e.Stage == telemetry.StageError && e.Fields["stage"] == "panic" {
			panicEvents++
		}
	}
	if panicEvents != 1 {
		t.Fatalf("panic-stage error events = %d, want 1", panicEvents)
	}
}

// TestSafeCycleRecoveredPanicSharesRunID is a regression test: the panic
// error event must carry the same run_id as the stage events the same
// cycle already emitted, not a fresh one generated only after recovery.
func TestSafeCycleRecoveredPanicSharesRunID(t *testing.T) {
	rec := &captureRecorder{}
	src := &fakeSource{commit: source.Commit{SHA: "abc123"}}
	obs := &observe.FakeObserver{}
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{}}, nil)
	app := &apply.FakeApplier{}

	deps := testDeps(src, spec.DesiredState{}, obs, app, rec)
	deps.Parse = func([]source.StackFile) (spec.DesiredState, error) {
		panic("boom")
	}

	if err := RunOnce(context.Background(), deps, testConfig(true)); err == nil {
		t.Fatal("RunOnce = nil error, want the recovered panic surfaced as an error")
	}

	events := rec.all()
	if len(events) < 2 {
		t.Fatalf("events = %+v, want at least a poll event and a panic error event", events)
	}
	first, last := events[0], events[len(events)-1]
	if first.RunID == "" || first.RunID != last.RunID {
		t.Fatalf("run_id mismatch: first event %q, last event %q", first.RunID, last.RunID)
	}
}

// TestCycleStageTimeoutBoundsHangingFetch is the production-hardening
// regression test: before cfg.StageTimeout existed, cycle() passed the
// process's own lifetime context straight through to Source.Fetch with no
// deadline of its own, so a remote that accepted a connection and never
// responded (a slow or interfered-with git host, in production) would
// hang the reconcile cycle forever. src.block is never closed here,
// simulating exactly that: Fetch only returns via <-ctx.Done().
func TestCycleStageTimeoutBoundsHangingFetch(t *testing.T) {
	rec := &captureRecorder{}
	src := &fakeSource{commit: source.Commit{SHA: "abc123"}, block: make(chan struct{})}
	obs := &observe.FakeObserver{}
	obs.Set(spec.ObservedState{Services: map[string]spec.ServiceSpec{}}, nil)
	app := &apply.FakeApplier{}

	deps := testDeps(src, spec.DesiredState{}, obs, app, rec)
	cfg := testConfig(true)
	cfg.StageTimeout = 20 * time.Millisecond

	start := time.Now()
	err := RunOnce(context.Background(), deps, cfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("RunOnce = nil error, want a timeout error from the hanging fetch")
	}
	if elapsed > time.Second {
		t.Fatalf("RunOnce took %v to return, want it bounded by StageTimeout (~20ms)", elapsed)
	}
}
