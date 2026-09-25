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

// driftDriftKinds is the exact --drift enum, in the same vocabulary
// driftKindForUpdate/emitDriftEvent (internal/loop/drift.go) attach to
// telemetry as fields["kind"].
var driftDriftKinds = map[string]bool{
	"replicas":  true,
	"image":     true,
	"env":       true,
	"removed":   true,
	"unmanaged": true,
}

// driftAPI is the slice of the Docker client drift needs to apply one direct,
// out-of-band mutation per run and (for the unmanaged case) confirm the
// decoy survived. Kept narrow, mirroring internal/apply's serviceAPI.
type driftAPI interface {
	ServiceInspectWithRaw(ctx context.Context, serviceID string, opts swarm.ServiceInspectOptions) (swarm.Service, []byte, error)
	ServiceUpdate(ctx context.Context, serviceID string, version swarm.Version, service swarm.ServiceSpec, options swarm.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error)
	ServiceRemove(ctx context.Context, serviceID string) error
	ServiceCreate(ctx context.Context, service swarm.ServiceSpec, options swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error)
}

// DriftConfig configures one drift-injection run.
type DriftConfig struct {
	Drift        string // one of driftDriftKinds
	Service      string // full qualified target service name; for unmanaged, the decoy's base name
	PollInterval time.Duration
	Registry     string // optional host[:port] prefix for the image drift/unmanaged cases' image (see imageRef in scale.go)
	EventsFile   string
	Timeout      time.Duration
	Label        string // folded verbatim into Condition, same convention as ScaleConfig.Label
}

func driftCondition(cfg DriftConfig) string {
	return fmt.Sprintf("drift=%s;service=%s;%s", cfg.Drift, cfg.Service, cfg.Label)
}

// driftRollbackImage is the fixed rollback target for the image-drift case.
//
// DESIGN NOTE: swarmgate always deploys digest-pinned images
// (repo@sha256:...), never a bare tag, so there is no reliable way to
// inspect a live service's current digest and reverse-map it to "the
// previous entry in scaleTags" at runtime. Rather than approximate that,
// image drift unconditionally retags to the first pinned tag in scaleTags —
// a fixed, deterministic rollback target standing in for "a previous
// pinned tag". This is a documented simplification of the original plan
// text ("retag to previous pinned tag"), not an attempt at true history.
func driftRollbackImage(registry string) string {
	return imageRef(registry, "nginx", scaleTags[0])
}

// driftDecoyName is the unmanaged case's decoy service name: unique per run so
// repeated runs do not collide.
func driftDecoyName(service string, run int) string {
	return fmt.Sprintf("%s-%d", service, run)
}

// driftSetEnv returns spec's Env with key=value set, replacing an existing
// entry for key if present and appending otherwise. Pure so the
// replace-vs-append behaviour is unit-testable without a live service.
func driftSetEnv(env []string, key, value string) []string {
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

// driftDriftMatch builds the AwaitEvent predicate for the detect phase: a
// drift event naming this exact service and kind, in the vocabulary
// internal/loop/drift.go emits.
func driftDriftMatch(kind, service string) func(telemetry.Event) bool {
	return func(e telemetry.Event) bool {
		return e.Stage == telemetry.StageDrift && e.Fields["kind"] == kind && e.Fields["service"] == service
	}
}

// driftAwaitRepair waits for the converged event belonging to the reconcile
// cycle (run_id) that applied a change to targetService, not the next
// converged event unfiltered (the former driftConvergedMatch contract), which
// could match a cycle that never touched the service. Each apply event for
// targetService updates the tracked run_id, so retries resolve correctly:
// only a converged event for the most recent apply can match.
func driftAwaitRepair(ctx context.Context, events <-chan telemetry.Event, targetService string) (telemetry.Event, error) {
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

// NewDriftDockerClient connects to the Docker engine at host, or via the
// standard environment when host is empty, matching the convention of
// observe.NewSwarmObserver and apply.NewSwarmApplier.
func NewDriftDockerClient(host string) (*client.Client, error) {
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

// RunDrift executes a drift run: n runs, each applying one direct
// Docker SDK mutation that bypasses git/swarmgate entirely, then measuring
// swarmgate's detection and (for four of the five kinds) repair. Each run
// writes two rows itself, so it loops directly rather than going through
// Run (whose scenario func returns exactly one Row per call).
func RunDrift(ctx context.Context, cfg DriftConfig, api driftAPI, n int, out *CSVWriter) error {
	if !driftDriftKinds[cfg.Drift] {
		return fmt.Errorf("drift: unknown drift kind %q", cfg.Drift)
	}
	for run := 1; run <= n; run++ {
		if err := driftRun(ctx, cfg, api, run, out); err != nil {
			return fmt.Errorf("run %d: %w", run, err)
		}
	}
	return nil
}

func driftRun(ctx context.Context, cfg DriftConfig, api driftAPI, run int, out *CSVWriter) error {
	targetService := cfg.Service
	if cfg.Drift == "unmanaged" {
		targetService = driftDecoyName(cfg.Service, run)
	}

	// waitCtx bounds only the event-waiting phases below by cfg.Timeout.
	// driftMutate and driftUnmanagedRepairRow keep the caller's own ctx: the
	// latter bounds its own wait (2*cfg.PollInterval) and must not be cut
	// short by cfg.Timeout too.
	waitCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	// Tail before mutate, matching Tail's documented contract and verify's
	// precedent. Opening the tail after the mutation risked missing an
	// event that landed in the gap.
	events, err := Tail(waitCtx, cfg.EventsFile)
	if err != nil {
		return out.Write(driftErrorRow(cfg, run, "tail events", err))
	}

	tStart := time.Now()
	if err := driftMutate(ctx, cfg, api, targetService, run); err != nil {
		return out.Write(driftErrorRow(cfg, run, "mutate", err))
	}

	detectEvent, err := awaitFromChannel(waitCtx, events, driftDriftMatch(cfg.Drift, targetService))
	detectRow := driftRow(cfg, run, "detect", tStart, detectEvent.T, err)
	if err != nil {
		return out.Write(detectRow)
	}
	if err := out.Write(detectRow); err != nil {
		return err
	}

	if cfg.Drift == "unmanaged" {
		return driftUnmanagedRepairRow(ctx, cfg, api, targetService, run, detectEvent.T, out)
	}

	convergedEvent, err := driftAwaitRepair(waitCtx, events, targetService)
	repairRow := driftRow(cfg, run, "repair", detectEvent.T, convergedEvent.T, err)
	return out.Write(repairRow)
}

// driftUnmanagedRepairRow implements the unmanaged case's repair check.
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
func driftUnmanagedRepairRow(ctx context.Context, cfg DriftConfig, api driftAPI, decoyName string, run int, tStart time.Time, out *CSVWriter) error {
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
		Scenario: "drift", Condition: driftCondition(cfg), Run: run,
		TStart: tStart, TEnd: tEnd, DurationMS: tEnd.Sub(tStart).Milliseconds(),
		Outcome: outcome, Detail: detail,
	})
}

// driftRow builds a detect or repair Row from an AwaitEvent outcome, classifying
// a deadline-exceeded error as "timeout" and anything else as "error". Detail
// is the fixed phase label ("detect"/"repair"); the error itself is not
// folded in, since Outcome already distinguishes timeout from failure.
func driftRow(cfg DriftConfig, run int, phase string, tStart, tEnd time.Time, err error) Row {
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
		Scenario: "drift", Condition: driftCondition(cfg), Run: run,
		TStart: tStart, TEnd: tEnd, DurationMS: tEnd.Sub(tStart).Milliseconds(),
		Outcome: outcome, Detail: phase,
	}
}

func driftErrorRow(cfg DriftConfig, run int, phase string, err error) Row {
	now := time.Now()
	return Row{
		Scenario: "drift", Condition: driftCondition(cfg), Run: run,
		TStart: now, TEnd: now, DurationMS: 0,
		Outcome: "error", Detail: phase + ";" + err.Error(),
	}
}

// driftMutate applies the one direct Docker SDK mutation for cfg.Drift against
// targetService, bypassing git/swarmgate entirely to simulate an operator's
// out-of-band change. run is the harness run index, used by the env and
// unmanaged cases to keep each run's mutation distinct.
func driftMutate(ctx context.Context, cfg DriftConfig, api driftAPI, targetService string, run int) error {
	switch cfg.Drift {
	case "replicas":
		return driftMutateReplicas(ctx, api, targetService)
	case "image":
		return driftMutateImage(ctx, api, targetService, cfg.Registry)
	case "env":
		return driftMutateEnv(ctx, api, targetService, run)
	case "removed":
		return driftMutateRemoved(ctx, api, targetService)
	case "unmanaged":
		return driftMutateUnmanaged(ctx, api, targetService, cfg.Registry)
	default:
		return fmt.Errorf("drift: unknown drift kind %q", cfg.Drift)
	}
}

func driftInspectForUpdate(ctx context.Context, api driftAPI, name string) (swarm.Service, error) {
	svc, _, err := api.ServiceInspectWithRaw(ctx, name, swarm.ServiceInspectOptions{})
	if err != nil {
		return swarm.Service{}, fmt.Errorf("inspect %q: %w", name, err)
	}
	return svc, nil
}

func driftMutateReplicas(ctx context.Context, api driftAPI, name string) error {
	svc, err := driftInspectForUpdate(ctx, api, name)
	if err != nil {
		return err
	}
	spec := svc.Spec
	if spec.Mode.Replicated == nil || spec.Mode.Replicated.Replicas == nil {
		return fmt.Errorf("drift: service %q is not in replicated mode", name)
	}
	n := *spec.Mode.Replicated.Replicas + 1
	spec.Mode.Replicated.Replicas = &n
	if _, err := api.ServiceUpdate(ctx, svc.ID, svc.Version, spec, swarm.ServiceUpdateOptions{}); err != nil {
		return fmt.Errorf("drift: update %q replicas: %w", name, err)
	}
	return nil
}

func driftMutateImage(ctx context.Context, api driftAPI, name, registry string) error {
	svc, err := driftInspectForUpdate(ctx, api, name)
	if err != nil {
		return err
	}
	spec := svc.Spec
	if spec.TaskTemplate.ContainerSpec == nil {
		return fmt.Errorf("drift: service %q has no container spec", name)
	}
	cs := *spec.TaskTemplate.ContainerSpec
	cs.Image = driftRollbackImage(registry)
	spec.TaskTemplate.ContainerSpec = &cs
	if _, err := api.ServiceUpdate(ctx, svc.ID, svc.Version, spec, swarm.ServiceUpdateOptions{}); err != nil {
		return fmt.Errorf("drift: update %q image: %w", name, err)
	}
	return nil
}

func driftMutateEnv(ctx context.Context, api driftAPI, name string, run int) error {
	svc, err := driftInspectForUpdate(ctx, api, name)
	if err != nil {
		return err
	}
	spec := svc.Spec
	if spec.TaskTemplate.ContainerSpec == nil {
		return fmt.Errorf("drift: service %q has no container spec", name)
	}
	cs := *spec.TaskTemplate.ContainerSpec
	cs.Env = driftSetEnv(cs.Env, "DRIFT", fmt.Sprintf("%d", run))
	spec.TaskTemplate.ContainerSpec = &cs
	if _, err := api.ServiceUpdate(ctx, svc.ID, svc.Version, spec, swarm.ServiceUpdateOptions{}); err != nil {
		return fmt.Errorf("drift: update %q env: %w", name, err)
	}
	return nil
}

func driftMutateRemoved(ctx context.Context, api driftAPI, name string) error {
	svc, err := driftInspectForUpdate(ctx, api, name)
	if err != nil {
		return err
	}
	if err := api.ServiceRemove(ctx, svc.ID); err != nil {
		return fmt.Errorf("drift: remove %q: %w", name, err)
	}
	return nil
}

// driftMutateUnmanaged creates a decoy service labelled managed=true (so
// swarmgate's observer, which server-side filters on that label, sees it)
// but deliberately without a stack label matching any real stack and never
// declared in any git stack file. That guarantees it never appears in
// desired state, so the reconciler classifies it as a Removes entry
// (drift kind "unmanaged") indefinitely.
//
// REQUIREMENT: see driftUnmanagedRepairRow — this scenario only makes sense
// against a swarmgate instance run with prune: false.
func driftMutateUnmanaged(ctx context.Context, api driftAPI, name, registry string) error {
	replicas := uint64(1)
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name:   name,
			Labels: map[string]string{spec.ManagedLabel: "true"},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{Image: driftRollbackImage(registry)},
		},
		Mode: swarm.ServiceMode{
			Replicated: &swarm.ReplicatedService{Replicas: &replicas},
		},
	}
	if _, err := api.ServiceCreate(ctx, spec, swarm.ServiceCreateOptions{}); err != nil {
		return fmt.Errorf("drift: create decoy %q: %w", name, err)
	}
	return nil
}
