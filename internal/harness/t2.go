package harness

import (
	"context"
	"errors"
	"fmt"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"

	"github.com/alexmchughdev/swarmgate/internal/spec"
	"github.com/alexmchughdev/swarmgate/internal/telemetry"
)

// t2DriftKinds is the exact --drift enum, in the same vocabulary
// driftKindForUpdate/emitDriftEvent (internal/loop/drift.go) attach to
// telemetry as fields["kind"].
var t2DriftKinds = map[string]bool{
	"replicas":  true,
	"image":     true,
	"env":       true,
	"removed":   true,
	"unmanaged": true,
}

// t2API is the slice of the Docker client t2 needs to apply one direct,
// out-of-band mutation per run and (for the unmanaged case) confirm the
// decoy survived. Kept narrow, mirroring internal/apply's serviceAPI.
type t2API interface {
	ServiceInspectWithRaw(ctx context.Context, serviceID string, opts swarm.ServiceInspectOptions) (swarm.Service, []byte, error)
	ServiceUpdate(ctx context.Context, serviceID string, version swarm.Version, service swarm.ServiceSpec, options swarm.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error)
	ServiceRemove(ctx context.Context, serviceID string) error
	ServiceCreate(ctx context.Context, service swarm.ServiceSpec, options swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error)
}

// T2Config configures one t2 drift-injection campaign.
type T2Config struct {
	Drift        string // one of t2DriftKinds
	Service      string // full qualified target service name; for unmanaged, the decoy's base name
	PollInterval time.Duration
	Registry     string // optional host[:port] prefix for the image drift/unmanaged cases' image (see imageRef in t1.go)
	EventsFile   string
	Timeout      time.Duration
	Label        string // folded verbatim into Condition, same convention as T1Config.Label
}

func t2Condition(cfg T2Config) string {
	return fmt.Sprintf("drift=%s;service=%s;%s", cfg.Drift, cfg.Service, cfg.Label)
}

// t2RollbackImage is the fixed rollback target for the image-drift case.
//
// DESIGN NOTE: swarmgate always deploys digest-pinned images
// (repo@sha256:...), never a bare tag, so there is no reliable way to
// inspect a live service's current digest and reverse-map it to "the
// previous entry in t1Tags" at runtime. Rather than approximate that,
// image drift unconditionally retags to the first pinned tag in t1Tags —
// a fixed, deterministic rollback target standing in for "a previous
// pinned tag". This is a documented simplification of the original plan
// text ("retag to previous pinned tag"), not an attempt at true history.
func t2RollbackImage(registry string) string {
	return imageRef(registry, "nginx", t1Tags[0])
}

// t2DecoyName is the unmanaged case's decoy service name: unique per run so
// repeated runs within one campaign do not collide.
func t2DecoyName(service string, run int) string {
	return fmt.Sprintf("%s-%d", service, run)
}

// t2SetEnv returns spec's Env with key=value set, replacing an existing
// entry for key if present and appending otherwise. Pure so the
// replace-vs-append behaviour is unit-testable without a live service.
func t2SetEnv(env []string, key, value string) []string {
	entry := key + "=" + value
	prefix := key + "="
	out := make([]string, len(env))
	copy(out, env)
	for i, e := range out {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			out[i] = entry
			return out
		}
	}
	return append(out, entry)
}

// t2DriftMatch builds the AwaitEvent predicate for the detect phase: a
// drift event naming this exact service and kind, in the vocabulary
// internal/loop/drift.go emits.
func t2DriftMatch(kind, service string) func(telemetry.Event) bool {
	return func(e telemetry.Event) bool {
		return e.Stage == telemetry.StageDrift && e.Fields["kind"] == kind && e.Fields["service"] == service
	}
}

// t2AwaitRepair waits for the converged event belonging to the reconcile
// cycle (run_id) that applied a change to targetService, not the next
// converged event unfiltered (the former t2ConvergedMatch contract), which
// could match a cycle that never touched the service. Each apply event for
// targetService updates the tracked run_id, so retries resolve correctly:
// only a converged event for the most recent apply can match.
func t2AwaitRepair(ctx context.Context, events <-chan telemetry.Event, targetService string) (telemetry.Event, error) {
	var applyRunID string
	for {
		select {
		case e, ok := <-events:
			if !ok {
				return telemetry.Event{}, fmt.Errorf("await repair: channel closed")
			}
			switch {
			case e.Stage == telemetry.StageApply && e.Service == targetService:
				applyRunID = e.RunID
			case e.Stage == telemetry.StageConverged && applyRunID != "" && e.RunID == applyRunID:
				return e, nil
			}
		case <-ctx.Done():
			return telemetry.Event{}, fmt.Errorf("await repair: %w", ctx.Err())
		}
	}
}

// NewT2DockerClient connects to the Docker engine at host, or via the
// standard environment when host is empty, matching the convention of
// observe.NewSwarmObserver and apply.NewSwarmApplier.
func NewT2DockerClient(host string) (*client.Client, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	c, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return c, nil
}

// RunT2 executes the t2 drift campaign: n runs, each applying one direct
// Docker SDK mutation that bypasses git/swarmgate entirely, then measuring
// swarmgate's detection and (for four of the five kinds) repair. Each run
// writes two rows itself, so it loops directly rather than going through
// Run (whose scenario func returns exactly one Row per call).
func RunT2(ctx context.Context, cfg T2Config, api t2API, n int, out *CSVWriter) error {
	if !t2DriftKinds[cfg.Drift] {
		return fmt.Errorf("t2: unknown drift kind %q", cfg.Drift)
	}
	for run := 1; run <= n; run++ {
		if err := t2Run(ctx, cfg, api, run, out); err != nil {
			return fmt.Errorf("run %d: %w", run, err)
		}
	}
	return nil
}

func t2Run(ctx context.Context, cfg T2Config, api t2API, run int, out *CSVWriter) error {
	targetService := cfg.Service
	if cfg.Drift == "unmanaged" {
		targetService = t2DecoyName(cfg.Service, run)
	}

	// waitCtx bounds only the event-waiting phases below by cfg.Timeout.
	// t2Mutate and t2UnmanagedRepairRow keep the caller's own ctx: the
	// latter bounds its own wait (2*cfg.PollInterval) and must not be cut
	// short by cfg.Timeout too.
	waitCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	// Tail before mutate, matching Tail's documented contract and t6's
	// precedent. Opening the tail after the mutation risked missing an
	// event that landed in the gap.
	events, err := Tail(waitCtx, cfg.EventsFile)
	if err != nil {
		return out.Write(t2ErrorRow(cfg, run, "tail events", err))
	}

	tStart := time.Now()
	if err := t2Mutate(ctx, cfg, api, targetService, run); err != nil {
		return out.Write(t2ErrorRow(cfg, run, "mutate", err))
	}

	detectEvent, err := awaitFromChannel(waitCtx, events, t2DriftMatch(cfg.Drift, targetService))
	detectRow := t2Row(cfg, run, "detect", tStart, detectEvent.T, err)
	if err != nil {
		return out.Write(detectRow)
	}
	if err := out.Write(detectRow); err != nil {
		return err
	}

	if cfg.Drift == "unmanaged" {
		return t2UnmanagedRepairRow(ctx, cfg, api, targetService, run, detectEvent.T, out)
	}

	convergedEvent, err := t2AwaitRepair(waitCtx, events, targetService)
	repairRow := t2Row(cfg, run, "repair", detectEvent.T, convergedEvent.T, err)
	return out.Write(repairRow)
}

// t2UnmanagedRepairRow implements the unmanaged case's repair check.
// Converged events fire even for a cycle that skipped every action (prune
// disabled), so their presence proves nothing about the decoy either way.
// Instead this waits out a fixed window and directly inspects the decoy:
// still present is the expected, correct outcome; gone means something
// removed it, which only happens if the swarmgate instance under test was
// misconfigured with prune enabled.
//
// REQUIREMENT: this drift kind is only meaningful against a swarmgate
// instance configured with prune: false. Under prune: true the decoy will
// actually be removed, which this scenario reports as an error but which
// is really just the harness being pointed at the wrong configuration.
func t2UnmanagedRepairRow(ctx context.Context, cfg T2Config, api t2API, decoyName string, run int, tStart time.Time, out *CSVWriter) error {
	waitFor := 2 * cfg.PollInterval
	elapsed := time.Since(tStart)
	if remaining := waitFor - elapsed; remaining > 0 {
		select {
		case <-time.After(remaining):
		case <-ctx.Done():
		}
	}
	tEnd := time.Now()

	_, _, err := api.ServiceInspectWithRaw(ctx, decoyName, swarm.ServiceInspectOptions{})
	outcome, detail := "ok", "repair;no_repair=true"
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			outcome, detail = "error", "repair;no_repair=false"
		} else {
			outcome, detail = "error", "repair;"+err.Error()
		}
	}
	return out.Write(Row{
		Scenario: "t2", Condition: t2Condition(cfg), Run: run,
		TStart: tStart, TEnd: tEnd, DurationMS: tEnd.Sub(tStart).Milliseconds(),
		Outcome: outcome, Detail: detail,
	})
}

// t2Row builds a detect or repair Row from an AwaitEvent outcome, classifying
// a deadline-exceeded error as "timeout" and anything else as "error". Detail
// is the fixed phase label ("detect"/"repair"); the error itself is not
// folded in, since Outcome already distinguishes timeout from failure.
func t2Row(cfg T2Config, run int, phase string, tStart, tEnd time.Time, err error) Row {
	outcome := "ok"
	if err != nil {
		tEnd = time.Now()
		if errors.Is(err, context.DeadlineExceeded) {
			outcome = "timeout"
		} else {
			outcome = "error"
		}
	}
	return Row{
		Scenario: "t2", Condition: t2Condition(cfg), Run: run,
		TStart: tStart, TEnd: tEnd, DurationMS: tEnd.Sub(tStart).Milliseconds(),
		Outcome: outcome, Detail: phase,
	}
}

func t2ErrorRow(cfg T2Config, run int, phase string, err error) Row {
	now := time.Now()
	return Row{
		Scenario: "t2", Condition: t2Condition(cfg), Run: run,
		TStart: now, TEnd: now, DurationMS: 0,
		Outcome: "error", Detail: phase + ";" + err.Error(),
	}
}

// t2Mutate applies the one direct Docker SDK mutation for cfg.Drift against
// targetService, bypassing git/swarmgate entirely to simulate an operator's
// out-of-band change. run is the harness run index, used by the env and
// unmanaged cases to keep each run's mutation distinct.
func t2Mutate(ctx context.Context, cfg T2Config, api t2API, targetService string, run int) error {
	switch cfg.Drift {
	case "replicas":
		return t2MutateReplicas(ctx, api, targetService)
	case "image":
		return t2MutateImage(ctx, api, targetService, cfg.Registry)
	case "env":
		return t2MutateEnv(ctx, api, targetService, run)
	case "removed":
		return t2MutateRemoved(ctx, api, targetService)
	case "unmanaged":
		return t2MutateUnmanaged(ctx, api, targetService, cfg.Registry)
	default:
		return fmt.Errorf("t2: unknown drift kind %q", cfg.Drift)
	}
}

func t2InspectForUpdate(ctx context.Context, api t2API, name string) (swarm.Service, error) {
	svc, _, err := api.ServiceInspectWithRaw(ctx, name, swarm.ServiceInspectOptions{})
	if err != nil {
		return swarm.Service{}, fmt.Errorf("inspect %q: %w", name, err)
	}
	return svc, nil
}

func t2MutateReplicas(ctx context.Context, api t2API, name string) error {
	svc, err := t2InspectForUpdate(ctx, api, name)
	if err != nil {
		return err
	}
	spec := svc.Spec
	if spec.Mode.Replicated == nil || spec.Mode.Replicated.Replicas == nil {
		return fmt.Errorf("t2: service %q is not in replicated mode", name)
	}
	n := *spec.Mode.Replicated.Replicas + 1
	spec.Mode.Replicated.Replicas = &n
	if _, err := api.ServiceUpdate(ctx, svc.ID, svc.Version, spec, swarm.ServiceUpdateOptions{}); err != nil {
		return fmt.Errorf("t2: update %q replicas: %w", name, err)
	}
	return nil
}

func t2MutateImage(ctx context.Context, api t2API, name, registry string) error {
	svc, err := t2InspectForUpdate(ctx, api, name)
	if err != nil {
		return err
	}
	spec := svc.Spec
	if spec.TaskTemplate.ContainerSpec == nil {
		return fmt.Errorf("t2: service %q has no container spec", name)
	}
	cs := *spec.TaskTemplate.ContainerSpec
	cs.Image = t2RollbackImage(registry)
	spec.TaskTemplate.ContainerSpec = &cs
	if _, err := api.ServiceUpdate(ctx, svc.ID, svc.Version, spec, swarm.ServiceUpdateOptions{}); err != nil {
		return fmt.Errorf("t2: update %q image: %w", name, err)
	}
	return nil
}

func t2MutateEnv(ctx context.Context, api t2API, name string, run int) error {
	svc, err := t2InspectForUpdate(ctx, api, name)
	if err != nil {
		return err
	}
	spec := svc.Spec
	if spec.TaskTemplate.ContainerSpec == nil {
		return fmt.Errorf("t2: service %q has no container spec", name)
	}
	cs := *spec.TaskTemplate.ContainerSpec
	cs.Env = t2SetEnv(cs.Env, "DRIFT", fmt.Sprintf("%d", run))
	spec.TaskTemplate.ContainerSpec = &cs
	if _, err := api.ServiceUpdate(ctx, svc.ID, svc.Version, spec, swarm.ServiceUpdateOptions{}); err != nil {
		return fmt.Errorf("t2: update %q env: %w", name, err)
	}
	return nil
}

func t2MutateRemoved(ctx context.Context, api t2API, name string) error {
	svc, err := t2InspectForUpdate(ctx, api, name)
	if err != nil {
		return err
	}
	if err := api.ServiceRemove(ctx, svc.ID); err != nil {
		return fmt.Errorf("t2: remove %q: %w", name, err)
	}
	return nil
}

// t2MutateUnmanaged creates a decoy service labelled managed=true (so
// swarmgate's observer, which server-side filters on that label, sees it)
// but deliberately without a stack label matching any real stack and never
// declared in any git stack file. That guarantees it never appears in
// desired state, so the reconciler classifies it as a Removes entry
// (drift kind "unmanaged") indefinitely.
//
// REQUIREMENT: see t2UnmanagedRepairRow — this scenario only makes sense
// against a swarmgate instance run with prune: false.
func t2MutateUnmanaged(ctx context.Context, api t2API, name, registry string) error {
	replicas := uint64(1)
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name:   name,
			Labels: map[string]string{spec.ManagedLabel: "true"},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{Image: t2RollbackImage(registry)},
		},
		Mode: swarm.ServiceMode{
			Replicated: &swarm.ReplicatedService{Replicas: &replicas},
		},
	}
	if _, err := api.ServiceCreate(ctx, spec, swarm.ServiceCreateOptions{}); err != nil {
		return fmt.Errorf("t2: create decoy %q: %w", name, err)
	}
	return nil
}
