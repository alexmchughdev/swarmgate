package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli/connhelper"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"gopkg.in/yaml.v3"

	"github.com/alexmchughdev/swarmgate/internal/source"
	"github.com/alexmchughdev/swarmgate/internal/spec"
	"github.com/alexmchughdev/swarmgate/internal/telemetry"
)

// faultConvergeTimeout bounds how long a fault run waits for the apply event that
// follows its push and, from there, for the matching converged event. Fixed
// rather than a flag: the plan calls for a long, per-run constant window,
// not an operator-tunable one.
const faultConvergeTimeout = 10 * time.Minute

// faultFinalVerifyTimeout bounds the one-time post-loop verify call used for
// the believed-state false_ok check. Unbounded here would have the same hang risk as
// faultPollLiveConverged's own per-check calls (see its comment): the target
// this run's DockerHost talks to may still be settling right after
// restore, and a bad connection attempt against it can hang rather than
// fail cleanly.
const faultFinalVerifyTimeout = 10 * time.Second

// faultReconcilerTarget is the conventional hosts-config key for killing the
// reconciler process itself (as opposed to a VM hosting it), matching the
// name used throughout this package's docs and examples. It gets a
// different await strategy in faultRunner.run; see the comment at its use.
const faultReconcilerTarget = "reconciler"

// faultPollLiveConvergePollInterval is the fixed cadence faultPollLiveConverged
// re-inspects live state at. Not a flag: this readout path exists only for
// the reconciler target's specific telemetry gap, not as a general-purpose
// tunable.
const faultPollLiveConvergePollInterval = 2 * time.Second

// faultPollLiveConverged directly inspects live state on a fixed cadence until
// it matches what was pushed, or deadline passes. It exists for targets
// where the normal converged-event wait can never succeed: see its call
// site in faultRunner.run. Unlike the event-driven path, "converged" here
// means "observed already at desired state," which may be true from the
// very first check if the underlying fault never actually diverged the
// live cluster from what was pushed.
func faultPollLiveConverged(ctx context.Context, cfg FaultConfig, stackContent string, deadline time.Time) (converged bool, at time.Time) {
	for {
		// Each check gets its own bounded timeout, not ctx directly: right
		// after a kill, the target this run's DockerHost talks to may be
		// mid-reboot, and a connection attempt against a host in that
		// state can hang rather than fail cleanly (no RST yet, no timeout
		// of its own). Without a per-check bound here, one such hang would
		// block the whole loop indefinitely regardless of deadline, since
		// deadline is only ever consulted between calls, never able to
		// interrupt one already in flight.
		checkCtx, cancel := context.WithTimeout(ctx, faultPollLiveConvergePollInterval)
		desired, observed, err := faultVerify(checkCtx, cfg, stackContent)
		cancel()
		if err == nil && reflect.DeepEqual(desired, observed) {
			return true, time.Now()
		}
		if !time.Now().Before(deadline) {
			return false, time.Time{}
		}
		select {
		case <-time.After(faultPollLiveConvergePollInterval):
		case <-ctx.Done():
			return false, time.Time{}
		}
	}
}

// hostAction is one role's fault-injection commands, run verbatim through a
// shell. The harness never parses or validates their content: they are
// opaque to it by design, since what a "kill" or "restore" means is entirely
// a property of the target infrastructure (a VM, a container, an rc
// service, ...).
type hostAction struct {
	Kill    string `yaml:"kill"`
	Restore string `yaml:"restore"`
}

// hostsConfig maps role names (manager, worker1, reconciler, or any
// operator-defined key) to their fault commands. There is no fixed enum of
// roles in code: the file's keys are the valid --target set.
type hostsConfig map[string]hostAction

// LoadHostsConfig reads and parses a hosts config file.
func LoadHostsConfig(path string) (hostsConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read hosts config %s: %w", path, err)
	}
	var cfg hostsConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse hosts config %s: %w", path, err)
	}
	return cfg, nil
}

// faultResolveTarget looks up target's host action. The config file's keys are
// the valid --target set, so this is the only validation --target gets;
// the error lists what was actually configured so a typo is obvious.
func faultResolveTarget(hosts hostsConfig, target string) (hostAction, error) {
	a, ok := hosts[target]
	if !ok {
		keys := make([]string, 0, len(hosts))
		for k := range hosts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return hostAction{}, fmt.Errorf("target %q not found in hosts config (known: %s)", target, strings.Join(keys, ", "))
	}
	return a, nil
}

// ValidateFaultTarget reports whether target is a configured role, so callers
// (the cmd/harness wiring) can fail fast on a bad --target before doing any
// other setup, mirroring how other scenarios validate flags before running.
func ValidateFaultTarget(hosts hostsConfig, target string) error {
	_, err := faultResolveTarget(hosts, target)
	return err
}

// FaultConfig configures one node/process fault-injection run.
type FaultConfig struct {
	Repo         string // path to an existing working-tree git clone
	Stack        string // stack name; file is <Stack>.yaml at the repo root
	Service      string // compose service key within the stack
	Hosts        hostsConfig
	Target       string // must be a key present in Hosts
	RestoreAfter time.Duration
	EventsFile   string
	DockerHost   string
	Label        string // expected form "events=on" / "events=off"; folded verbatim into Condition
	Registry     string // prefixed onto Image via imageRef; empty preserves the original nginx-only behavior
	Image        string // empty defaults to "nginx"
	Tags         []string
	// PollReadout forces the direct-live-state-poll convergence readout
	// (see faultPollLiveConverged) instead of waiting on the events stream.
	// Always true for target "reconciler" regardless of this field (a
	// structural property of that target: see the comment at its use).
	// Set this explicitly for other targets whose events file is itself
	// relayed across the same fault being injected (e.g. a manager-kill
	// run observed from a different host over a connection that the kill
	// necessarily disrupts) — the specific converged event for a run can
	// be lost in that gap even though the target itself recovers cleanly,
	// which is an environmental/topology concern distinct from the
	// reconciler target's always-true structural one.
	PollReadout bool
}

func faultCondition(target string, restoreAfter time.Duration, label string) string {
	return fmt.Sprintf("target=%s;restore_after=%s;%s", target, restoreAfter, label)
}

// faultFalseOK implements the believed-state false-ok predicate: a run is false_ok only
// when the reconciler reported convergence and an independent inspection of
// the live service disagrees with what was pushed. "Never converged" is an
// honest failure, not a false ok, so it is excluded up front.
func faultFalseOK(desired, observed spec.ServiceSpec, converged bool) bool {
	if !converged {
		return false
	}
	return !reflect.DeepEqual(desired, observed)
}

// faultRunner carries one run's fixed configuration; unlike scale it has no
// cross-run mutable state beyond the run index itself (the image tag cycle
// is a pure function of runIdx).
type faultRunner struct {
	cfg    FaultConfig
	action hostAction
}

func newFaultRunner(cfg FaultConfig, action hostAction) *faultRunner {
	return &faultRunner{cfg: cfg, action: action}
}

func (r *faultRunner) run(runIdx int) Row {
	cfg := r.cfg
	condition := faultCondition(cfg.Target, cfg.RestoreAfter, cfg.Label)

	tags := cfg.Tags
	if len(tags) == 0 {
		tags = scaleTags
	}
	image := cfg.Image
	if image == "" {
		image = "nginx"
	}
	tag := tags[runIdx%len(tags)]
	service := scaleService{Name: cfg.Service, Image: imageRef(cfg.Registry, image, tag)}
	stackContent := scaleStackYAML([]scaleService{service})
	stackPath := filepath.Join(cfg.Repo, cfg.Stack+".yaml")
	if err := os.WriteFile(stackPath, []byte(stackContent), 0o644); err != nil {
		return faultErrorRow(runIdx, condition, fmt.Errorf("write stack file: %w", err))
	}

	// Guaranteed-once restore: the sync.Once ensures whichever of (a) the
	// timer fired via --restore-after, or (b) this deferred call, runs
	// first is the only one that actually executes the restore command.
	// The defer is the backstop for restore-after==0 and for "the timer
	// never got the chance to fire" (early return, panic, ...).
	var restoreOnce sync.Once
	var restoreErr error
	restore := func() {
		restoreOnce.Do(func() {
			if r.action.Restore == "" {
				return
			}
			if err := faultRunShell(context.Background(), r.action.Restore); err != nil {
				restoreErr = err
				fmt.Fprintf(os.Stderr, "fault run %d: restore failed for target %s: %v\n", runIdx, cfg.Target, err)
			}
		})
	}
	defer restore()

	// Tail before the push that will produce the apply event this run
	// reacts to, so the race is won by construction rather than by luck.
	waitCtx, cancel := context.WithTimeout(context.Background(), faultConvergeTimeout)
	defer cancel()
	waitStart := time.Now()
	events, err := Tail(waitCtx, cfg.EventsFile)
	if err != nil {
		return faultErrorRow(runIdx, condition, fmt.Errorf("tail events: %w", err))
	}

	sha, err := gitCommitAndPush(cfg.Repo, cfg.Stack+".yaml", fmt.Sprintf("fault run %d", runIdx), true)
	if err != nil {
		return faultErrorRow(runIdx, condition, err)
	}

	deadline := waitStart.Add(faultConvergeTimeout)
	var (
		killFired  bool
		tKill      time.Time
		killErr    error
		converged  bool
		tConverged time.Time
	)
	for e := range events {
		if !killFired && e.Stage == telemetry.StageApply {
			killFired = true
			tKill = time.Now()
			if err := faultRunShell(context.Background(), r.action.Kill); err != nil {
				killErr = err
			}
			if cfg.RestoreAfter > 0 {
				time.AfterFunc(cfg.RestoreAfter, restore)
			}
			if cfg.Target == faultReconcilerTarget || cfg.PollReadout {
				// The apply this run reacts to has already happened by the
				// time this event fires, so the fault lands on an
				// already-converged commit. Swarmgate's own loop only
				// emits a converged event for a cycle that changed
				// something (loop.go: "converged is reserved for cycles
				// that changed something"), so the restarted process's
				// next poll finds an empty diff and never re-emits one for
				// this commit — no telemetry event will ever confirm this
				// run's convergence. Poll live state directly instead.
				//
				// The same dead end applies whenever PollReadout is forced
				// for a non-reconciler reason too (see its doc comment):
				// once the specific converged event is missed, an
				// already-stable commit's later empty-diff cycles won't
				// re-announce it regardless of why it was missed.
				converged, tConverged = faultPollLiveConverged(context.Background(), cfg, stackContent, deadline)
				break
			}
			continue
		}
		if killFired && e.Stage == telemetry.StageConverged && e.Fields["commit"] == sha {
			converged = true
			tConverged = e.T
			break
		}
	}

	outcome := "ok"
	tEnd := tConverged
	if !converged {
		outcome = "timeout"
		tEnd = deadline
	}
	if !killFired {
		// No apply event was observed at all: the run still completes and
		// reports rather than being discarded, since "the fault was never
		// injected" is itself informative.
		tKill = waitStart
	}

	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), faultFinalVerifyTimeout)
	desired, observed, verifyErr := faultVerify(verifyCtx, cfg, stackContent)
	verifyCancel()
	falseOK := false
	if verifyErr == nil {
		falseOK = faultFalseOK(desired, observed, converged)
	}

	readout := "event"
	if (cfg.Target == faultReconcilerTarget || cfg.PollReadout) && killFired {
		readout = "poll"
	}
	detail := fmt.Sprintf("false_ok=%t;readout=%s", falseOK, readout)
	if killErr != nil {
		detail += fmt.Sprintf(";kill_error=%s", killErr.Error())
	}
	if !killFired {
		detail += ";kill_skipped=apply event not observed"
	}
	if verifyErr != nil {
		detail += fmt.Sprintf(";verify_error=%s", verifyErr.Error())
	}
	if restoreErr != nil {
		detail += fmt.Sprintf(";restore_error=%s", restoreErr.Error())
	}

	return Row{
		Scenario: "fault", Condition: condition, Run: runIdx,
		TStart: tKill, TEnd: tEnd, DurationMS: tEnd.Sub(tKill).Milliseconds(),
		Outcome: outcome, Detail: detail,
	}
}

// faultVerify independently checks the live cluster against the stack file
// this run pushed, for the false_ok comparison. It re-parses the
// pushed content (rather than trusting anything observed mid-run) and
// inspects the single service this scenario manages.
//
// The desired side is resolved to a digest exactly as the reconcile loop
// itself does (parse, then resolve) before comparing: the observed side
// always reflects whatever the applier actually submitted, which is
// digest-pinned. Comparing an unresolved tag against a digest would flag
// every genuinely converged run as a mismatch, making false_ok trivially
// true always — resolving first is what makes this an honest check of the
// believed-state claim rather than a tautology.
func faultVerify(ctx context.Context, cfg FaultConfig, stackContent string) (desired, observed spec.ServiceSpec, err error) {
	ds, err := spec.Parse([]source.StackFile{{Name: cfg.Stack, Content: []byte(stackContent)}}, nil)
	if err != nil {
		return spec.ServiceSpec{}, spec.ServiceSpec{}, fmt.Errorf("parse pushed stack: %w", err)
	}
	if err := spec.Resolve(ctx, &ds, ""); err != nil {
		return spec.ServiceSpec{}, spec.ServiceSpec{}, fmt.Errorf("resolve pushed stack: %w", err)
	}
	qualified := spec.ServiceName(cfg.Stack, cfg.Service)
	desired, ok := ds.Services[qualified]
	if !ok {
		return spec.ServiceSpec{}, spec.ServiceSpec{}, fmt.Errorf("service %q not found in parsed stack", qualified)
	}

	api, err := newFaultDockerClient(cfg.DockerHost)
	if err != nil {
		return desired, spec.ServiceSpec{}, err
	}
	nets, err := api.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return desired, spec.ServiceSpec{}, fmt.Errorf("list networks: %w", err)
	}
	names := make(map[string]string, len(nets))
	for _, n := range nets {
		names[n.ID] = n.Name
	}

	svc, _, err := api.ServiceInspectWithRaw(ctx, qualified, swarm.ServiceInspectOptions{})
	if err != nil {
		return desired, spec.ServiceSpec{}, fmt.Errorf("inspect service %s: %w", qualified, err)
	}
	observed = spec.FromSwarm(svc, names)
	return desired, observed, nil
}

// faultDockerAPI is the slice of the Docker client faultVerify needs, kept narrow
// so it can be satisfied by *client.Client without pulling in the rest of
// the SDK's surface into this package's dependency graph.
type faultDockerAPI interface {
	ServiceInspectWithRaw(ctx context.Context, serviceID string, opts swarm.ServiceInspectOptions) (swarm.Service, []byte, error)
	NetworkList(ctx context.Context, options network.ListOptions) ([]network.Summary, error)
}

// newFaultDockerClient builds a Docker API client for faultVerify. host may be an
// ssh://user@host[/path] URL: fault's verify step often needs to reach a
// manager other than the one the harness process itself runs on (killing
// the harness's own host mid-run isn't observable from inside it), so the
// same connection helper `docker context`/`docker -H ssh://...` uses is
// wired in here rather than relying on client.WithHost's native schemes
// (tcp/unix/npipe only).
func newFaultDockerClient(host string) (faultDockerAPI, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		if helper, err := connhelper.GetConnectionHelper(host); err == nil && helper != nil {
			opts = append(opts,
				client.WithHost(helper.Host),
				client.WithDialContext(helper.Dialer),
			)
		} else {
			opts = append(opts, client.WithHost(host))
		}
	}
	c, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return c, nil
}

func faultErrorRow(runIdx int, condition string, err error) Row {
	now := time.Now()
	return Row{
		Scenario: "fault", Condition: condition, Run: runIdx,
		TStart: now, TEnd: now, DurationMS: 0, Outcome: "error", Detail: err.Error(),
	}
}

// faultRunShell executes an opaque fault-injection command through a shell.
// The harness treats its content as a black box: only whether it exits
// non-zero is meaningful.
func faultRunShell(ctx context.Context, cmd string) error {
	if strings.TrimSpace(cmd) == "" {
		return fmt.Errorf("empty command")
	}
	if err := exec.CommandContext(ctx, "sh", "-c", cmd).Run(); err != nil {
		return fmt.Errorf("run %q: %w", cmd, err)
	}
	return nil
}

// RunFault executes a fault-injection run: n iterations, each pushing a
// one-service stack change, killing --target on the first apply event that
// follows, awaiting convergence, restoring the target, and independently
// verifying the live service against what was pushed.
func RunFault(cfg FaultConfig, n int, out *CSVWriter) error {
	action, err := faultResolveTarget(cfg.Hosts, cfg.Target)
	if err != nil {
		return err
	}
	r := newFaultRunner(cfg, action)
	return Run(out, n, r.run)
}
