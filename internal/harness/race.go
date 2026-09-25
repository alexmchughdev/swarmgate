package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"

	"github.com/alexmchughdev/swarmgate/internal/spec"
	"github.com/alexmchughdev/swarmgate/internal/telemetry"
)

// RaceOffsets are the valid --offset values: the point in swarmgate's
// reconcile cycle, relative to the operator's out-of-band mutation, that a
// run measures.
var RaceOffsets = []string{"diff", "window", "apply", "after"}

// raceEnvKey is the env var the operator mutation sets, and the marker
// raceCheckWinner looks for when deciding who won the race.
const raceEnvKey = "TOCTOU"

// raceVerifyTimeout bounds the final winner-inspect call. It is independent
// of a run's own --timeout: verification must still happen (best-effort)
// even after a run has already timed out waiting for convergence.
const raceVerifyTimeout = 30 * time.Second

// raceMutationTimeout bounds the operator mutation's inspect+update round
// trip. Independent of the run's own deadline so a trigger that fires right
// as that deadline expires does not have its mutation call spuriously
// cancelled by the harness's own orchestration.
const raceMutationTimeout = 30 * time.Second

// RaceConfig configures one TOCTOU race run.
type RaceConfig struct {
	Repo       string // path to an existing working-tree clone
	Stack      string // stack name; file is <Stack>.yaml at the repo root
	Service    string // compose service key within the stack (bare, e.g. "web1" — not stack-qualified; see spec.ServiceName for how it's qualified internally)
	Offset     string // one of RaceOffsets
	DockerHost string // docker engine host for the operator mutation and verification
	EventsFile string
	Timeout    time.Duration
	Label      string // expected form "events=on" / "events=off"; folded verbatim into Condition
	Registry   string // prefixed onto Image via imageRef; empty preserves the original nginx-only behavior
	Image      string // empty defaults to "nginx"
	Tags       []string
}

func raceCondition(cfg RaceConfig) string {
	return fmt.Sprintf("offset=%s;service=%s;%s", cfg.Offset, cfg.Service, cfg.Label)
}

// raceShouldFireOnEvent reports whether e is the trigger point for offset,
// racing the operator's mutation against swarmgate's own reconcile of
// service.
//
// diff and window are implemented identically: swarmgate emits no signal
// between "diff computed" and "the first apply call about to start" (see
// internal/loop/loop.go, StageDiff through StageApply), so the diff event
// is the only observable boundary marking that window's start. There is no
// finer-grained trigger to fire "window" on than "fire on diff" — this is
// intentional, not a placeholder.
func raceShouldFireOnEvent(offset string, e telemetry.Event, service string) bool {
	switch offset {
	case "diff", "window":
		return e.Stage == telemetry.StageDiff
	case "apply":
		return e.Stage == telemetry.StageApply && e.Service == service
	case "after":
		return e.Stage == telemetry.StageConverged
	default:
		return false
	}
}

// raceDetail composes the Row.Detail string from a run's outcome. revertMS is
// included only when hasRevert is true (winner == "git" and a drift event
// was observed before the settling converged event).
func raceDetail(winner string, detected bool, revertMS int64, hasRevert bool) string {
	d := fmt.Sprintf("winner=%s;detected=%t", winner, detected)
	if hasRevert {
		d += fmt.Sprintf(";revert_ms=%d", revertMS)
	}
	return d
}

// raceAPI is the slice of the Docker client race actually uses: an inspect to
// read current version/spec plus an update, kept narrow to mirror
// internal/apply's serviceAPI so tests could substitute it without a live
// daemon.
type raceAPI interface {
	ServiceInspectWithRaw(ctx context.Context, serviceID string, opts swarm.ServiceInspectOptions) (swarm.Service, []byte, error)
	ServiceUpdate(ctx context.Context, serviceID string, version swarm.Version, service swarm.ServiceSpec, options swarm.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error)
}

// newRaceClient connects to the Docker engine at host, or via the standard
// environment when host is empty.
func newRaceClient(host string) (raceAPI, error) {
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

// raceSetOperatorEnv performs the operator's out-of-band mutation: it inspects
// service for its current version/spec, appends key=value to the container
// env, and updates. This bypasses git entirely, which is the point.
func raceSetOperatorEnv(ctx context.Context, api raceAPI, service, key, value string) error {
	current, _, err := api.ServiceInspectWithRaw(ctx, service, swarm.ServiceInspectOptions{})
	if err != nil {
		return fmt.Errorf("operator mutation: inspect %q: %w", service, err)
	}
	mutated := current.Spec
	cs := swarm.ContainerSpec{}
	if mutated.TaskTemplate.ContainerSpec != nil {
		cs = *mutated.TaskTemplate.ContainerSpec
	}
	cs.Env = append(append([]string(nil), cs.Env...), fmt.Sprintf("%s=%s", key, value))
	mutated.TaskTemplate.ContainerSpec = &cs
	if _, err := api.ServiceUpdate(ctx, current.ID, current.Version, mutated, swarm.ServiceUpdateOptions{}); err != nil {
		return fmt.Errorf("operator mutation: update %q: %w", service, err)
	}
	return nil
}

// raceCheckWinner inspects service's live spec and reports "operator" if it
// still carries key=value, "git" otherwise (meaning swarmgate's desired
// state, which never sets key, is what's live).
func raceCheckWinner(ctx context.Context, api raceAPI, service, key, value string) (string, error) {
	svc, _, err := api.ServiceInspectWithRaw(ctx, service, swarm.ServiceInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("verify: inspect %q: %w", service, err)
	}
	want := key + "=" + value
	if svc.Spec.TaskTemplate.ContainerSpec != nil {
		for _, e := range svc.Spec.TaskTemplate.ContainerSpec.Env {
			if e == want {
				return "operator", nil
			}
		}
	}
	return "git", nil
}

// raceRunner carries the Docker client across a run's iterations.
type raceRunner struct {
	cfg RaceConfig
	api raceAPI
}

func newRaceRunner(cfg RaceConfig, api raceAPI) *raceRunner {
	return &raceRunner{cfg: cfg, api: api}
}

// run executes one race: push a legitimate git change to cfg.Service, fire
// an out-of-band operator mutation at the configured offset relative to
// swarmgate's reconcile of that push, then watch for convergence and
// inspect who won.
func (r *raceRunner) run(runIdx int) Row {
	cfg := r.cfg
	// cfg.Service is the bare compose key used inside the generated stack
	// (scaleStackYAML below); everywhere else — API calls, telemetry event
	// comparisons — needs the stack-qualified name swarmgate itself uses.
	qualified := spec.ServiceName(cfg.Stack, cfg.Service)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	// Tail before push: the trigger point is an event from the SAME cycle
	// the push causes, so listening must already be in place before the
	// push exists to be reconciled.
	events, err := Tail(ctx, cfg.EventsFile)
	if err != nil {
		return raceErrorRow(runIdx, cfg, fmt.Errorf("tail events: %w", err))
	}

	tags := cfg.Tags
	if len(tags) == 0 {
		tags = scaleTags
	}
	image := cfg.Image
	if image == "" {
		image = "nginx"
	}
	stackPath := filepath.Join(cfg.Repo, cfg.Stack+".yaml")
	stack := scaleStackYAML([]scaleService{{Name: cfg.Service, Image: imageRef(cfg.Registry, image, tags[runIdx%len(tags)])}})
	if err := os.WriteFile(stackPath, []byte(stack), 0o644); err != nil {
		return raceErrorRow(runIdx, cfg, fmt.Errorf("write stack file: %w", err))
	}
	if _, err := gitCommitAndPush(cfg.Repo, cfg.Stack+".yaml", fmt.Sprintf("race run %d", runIdx), true); err != nil {
		return raceErrorRow(runIdx, cfg, err)
	}

	// Phase A: wait for the configured offset's trigger point, discarding
	// unrelated events in between.
	fired := false
	for !fired {
		select {
		case e, ok := <-events:
			if !ok {
				now := time.Now()
				return r.settle(runIdx, now, now, false, false, time.Time{})
			}
			if raceShouldFireOnEvent(cfg.Offset, e, qualified) {
				fired = true
			}
		case <-ctx.Done():
			now := time.Now()
			return r.settle(runIdx, now, now, false, false, time.Time{})
		}
	}

	mutationCtx, mutationCancel := context.WithTimeout(context.Background(), raceMutationTimeout)
	envValue := fmt.Sprintf("%d", runIdx)
	err = raceSetOperatorEnv(mutationCtx, r.api, qualified, raceEnvKey, envValue)
	mutationCancel()
	if err != nil {
		return raceErrorRow(runIdx, cfg, err)
	}
	tStart := time.Now()

	// Phase B: keep draining the same channel (do not re-Tail) until
	// convergence or timeout, tracking any drift event mentioning the
	// service along the way.
	detected := false
	var driftAt time.Time
	converged := false
	tEnd := tStart
drain:
	for {
		select {
		case e, ok := <-events:
			if !ok {
				tEnd = time.Now()
				break drain
			}
			if e.Stage == telemetry.StageDrift && e.Service == qualified && !detected {
				detected = true
				driftAt = e.T
			}
			if e.Stage == telemetry.StageConverged {
				converged = true
				tEnd = e.T
				break drain
			}
		case <-ctx.Done():
			tEnd = time.Now()
			break drain
		}
	}

	return r.settle(runIdx, tStart, tEnd, converged, detected, driftAt)
}

// settle performs the independent post-hoc verification (step 5) and
// assembles the Row. Called both when a run genuinely converges/times out
// and, best-effort, when the trigger event itself never arrived.
func (r *raceRunner) settle(runIdx int, tStart, tEnd time.Time, converged, detected bool, driftAt time.Time) Row {
	verifyCtx, cancel := context.WithTimeout(context.Background(), raceVerifyTimeout)
	defer cancel()

	envValue := fmt.Sprintf("%d", runIdx)
	qualified := spec.ServiceName(r.cfg.Stack, r.cfg.Service)
	winner, err := raceCheckWinner(verifyCtx, r.api, qualified, raceEnvKey, envValue)
	if err != nil {
		// Verification failing is not one of the defined hard-failure paths
		// (push, operator mutation call) — report best-effort rather than
		// discarding the run's timing data.
		winner = "unknown"
	}

	var revertMS int64
	hasRevert := detected && converged && winner == "git" && !driftAt.IsZero()
	if hasRevert {
		revertMS = tEnd.Sub(driftAt).Milliseconds()
	}

	outcome := "timeout"
	if converged {
		outcome = "ok"
	}

	return Row{
		Scenario: "race", Condition: raceCondition(r.cfg), Run: runIdx,
		TStart: tStart, TEnd: tEnd, DurationMS: tEnd.Sub(tStart).Milliseconds(),
		Outcome: outcome, Detail: raceDetail(winner, detected, revertMS, hasRevert),
	}
}

func raceErrorRow(runIdx int, cfg RaceConfig, err error) Row {
	now := time.Now()
	return Row{
		Scenario: "race", Condition: raceCondition(cfg), Run: runIdx,
		TStart: now, TEnd: now, DurationMS: 0, Outcome: "error", Detail: err.Error(),
	}
}

// RunRace executes a TOCTOU race run: n iterations, each racing an
// operator's direct Docker SDK mutation against swarmgate's git-driven
// reconcile of the same service at the configured offset.
func RunRace(cfg RaceConfig, n int, out *CSVWriter) error {
	api, err := newRaceClient(cfg.DockerHost)
	if err != nil {
		return err
	}
	r := newRaceRunner(cfg, api)
	return Run(out, n, r.run)
}
