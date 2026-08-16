package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/telemetry"
)

// t5Iface is the default network interface tc operates on at the registry
// host; overridable via T5Config.Iface.
const t5Iface = "eth0"

// t5RegistryPort is the registry's TCP port, blocked by the "unreachable"
// condition.
const t5RegistryPort = 5000

// T5Config configures one t5 registry-latency campaign.
type T5Config struct {
	Repo         string // path to an existing working-tree clone
	Stack        string // stack name; file is <Stack>.yaml at the repo root
	Service      string // service name bumped each run
	RegistryHost string // ssh target, e.g. "user@registry-host"
	Iface        string // interface tc operates on at the registry host
	Latency      string // raw --latency flag value, folded into Condition
	Push         bool   // push after commit; false = commit only
	EventsFile   string
	Timeout      time.Duration
	Label        string // expected form "events=on" / "events=off"; folded verbatim into Condition
	// ImageRegistry is the host[:port] prefix (via imageRef) for the pushed
	// service's image — distinct from RegistryHost, which is the ssh target
	// for the tc/iptables commands. Empty preserves the original
	// nginx-from-Docker-Hub behavior, but that default defeats this
	// scenario's own premise for the "unreachable" condition (an iptables
	// DROP scoped to RegistryHost's port t5RegistryPort has no effect on a
	// pull that never talks to that host): set ImageRegistry to
	// RegistryHost's own address so the pushed service actually pulls from
	// the host the network condition is being injected at.
	ImageRegistry string
	Image         string // empty defaults to "nginx"
	Tags          []string
}

// t5ParseLatency parses the --latency flag. "unreachable" selects the
// iptables DROP condition; anything else must parse as a time.Duration,
// including "0" (parses to a zero duration meaning "no injected latency" —
// the baseline/control condition, where no tc command is applied at all).
func t5ParseLatency(s string) (d time.Duration, unreachable bool, err error) {
	if s == "unreachable" {
		return 0, true, nil
	}
	d, err = time.ParseDuration(s)
	if err != nil {
		return 0, false, fmt.Errorf("invalid --latency %q: %w", s, err)
	}
	return d, false, nil
}

// t5NetemCommand returns the tc command that injects delay on iface. delay
// is formatted with time.Duration.String, which produces unit suffixes
// (e.g. "500ms", "5s") that tc's netem qdisc also accepts for these
// magnitudes.
func t5NetemCommand(iface string, delay time.Duration) string {
	return fmt.Sprintf("tc qdisc replace dev %s root netem delay %s", iface, delay.String())
}

// t5NetemDeleteCommand returns the tc command that removes a netem qdisc
// previously installed by t5NetemCommand.
func t5NetemDeleteCommand(iface string) string {
	return fmt.Sprintf("tc qdisc del dev %s root netem", iface)
}

// t5DropCommand returns the iptables command that drops inbound traffic to
// the registry port, simulating an unreachable registry. Targets
// DOCKER-USER rather than INPUT: when the registry runs as a Docker
// container with a published port (the expected deployment shape here),
// inbound connections are DNAT'd and forwarded into the container's
// network namespace rather than delivered to a host-local socket, so they
// never traverse INPUT at all — confirmed by direct testing (an INPUT DROP
// rule left registry access completely unaffected; DOCKER-USER blocked it
// immediately). DOCKER-USER is the chain Docker itself guarantees is
// evaluated before its own NAT/forwarding rules, specifically so operators
// have a stable hook for this kind of filtering.
func t5DropCommand(port int) string {
	return fmt.Sprintf("iptables -I DOCKER-USER -p tcp --dport %d -j DROP", port)
}

// t5DropDeleteCommand returns the iptables command that removes the DROP
// rule previously installed by t5DropCommand. Same rule spec as the insert,
// with -D in place of -I.
func t5DropDeleteCommand(port int) string {
	return fmt.Sprintf("iptables -D DOCKER-USER -p tcp --dport %d -j DROP", port)
}

// t5Condition renders the Condition column for a t5 row.
func t5Condition(latency, service, label string) string {
	return fmt.Sprintf("latency=%s;service=%s;%s", latency, service, label)
}

// t5RunSSH runs command on host over ssh, wrapping any output into the
// returned error so a broken ssh setup (bad host, missing tc/iptables,
// insufficient privilege) is diagnosable from the harness's own output.
func t5RunSSH(ctx context.Context, host, command string) error {
	out, err := exec.CommandContext(ctx, "ssh", host, command).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh %s %q: %w: %s", host, command, err, out)
	}
	return nil
}

// t5Apply installs the network condition described by cfg at the registry
// host. The zero-latency case is a control/baseline condition: it applies
// nothing, so there is no tc invocation and (see t5Restore) nothing to
// restore either.
func t5Apply(ctx context.Context, cfg T5Config) error {
	d, unreachable, err := t5ParseLatency(cfg.Latency)
	if err != nil {
		return err
	}
	switch {
	case unreachable:
		return t5RunSSH(ctx, cfg.RegistryHost, t5DropCommand(t5RegistryPort))
	case d == 0:
		return nil
	default:
		return t5RunSSH(ctx, cfg.RegistryHost, t5NetemCommand(cfg.Iface, d))
	}
}

// t5Restore removes the network condition previously installed by t5Apply.
// Best-effort: a "tc qdisc del" on an interface with no netem qdisc
// commonly exits non-zero because there is nothing to delete, so failures
// here are logged rather than returned, and never mask run results already
// recorded.
func t5Restore(ctx context.Context, cfg T5Config) {
	_, unreachable, err := t5ParseLatency(cfg.Latency)
	if err != nil {
		// Already reported by t5Apply's failure; nothing to restore.
		return
	}
	switch {
	case unreachable:
		if err := t5RunSSH(ctx, cfg.RegistryHost, t5DropDeleteCommand(t5RegistryPort)); err != nil {
			fmt.Fprintf(os.Stderr, "t5: restore (iptables) failed: %v\n", err)
		}
	default:
		d, _, _ := t5ParseLatency(cfg.Latency)
		if d == 0 {
			return // control condition: nothing was applied
		}
		if err := t5RunSSH(ctx, cfg.RegistryHost, t5NetemDeleteCommand(cfg.Iface)); err != nil {
			fmt.Fprintf(os.Stderr, "t5: restore (tc) failed: %v\n", err)
		}
	}
}

func t5ErrorRow(runIdx int, cfg T5Config, err error) Row {
	now := time.Now()
	return Row{
		Scenario: "t5", Condition: t5Condition(cfg.Latency, cfg.Service, cfg.Label), Run: runIdx,
		TStart: now, TEnd: now, DurationMS: 0, Outcome: "error", Detail: err.Error(),
	}
}

// t5Runner carries cross-run state (the per-run image tag cycle) for one
// campaign.
type t5Runner struct {
	cfg T5Config
}

func newT5Runner(cfg T5Config) *t5Runner {
	return &t5Runner{cfg: cfg}
}

func (r *t5Runner) run(runIdx int) Row {
	tags := r.cfg.Tags
	if len(tags) == 0 {
		tags = t1Tags
	}
	imageName := r.cfg.Image
	if imageName == "" {
		imageName = "nginx"
	}
	image := imageRef(r.cfg.ImageRegistry, imageName, tags[runIdx%len(tags)])
	stackPath := filepath.Join(r.cfg.Repo, r.cfg.Stack+".yaml")
	stack := t1StackYAML([]t1Service{{Name: r.cfg.Service, Image: image}})
	if err := os.WriteFile(stackPath, []byte(stack), 0o644); err != nil {
		return t5ErrorRow(runIdx, r.cfg, fmt.Errorf("write stack file: %w", err))
	}

	sha, err := gitCommitAndPush(r.cfg.Repo, r.cfg.Stack+".yaml", fmt.Sprintf("t5 run %d", runIdx), r.cfg.Push)
	if err != nil {
		return t5ErrorRow(runIdx, r.cfg, err)
	}

	tStart := time.Now()
	match := func(e telemetry.Event) bool {
		return e.Stage == telemetry.StageConverged && e.Fields["commit"] == sha
	}
	e, err := AwaitEvent(context.Background(), r.cfg.EventsFile, match, r.cfg.Timeout)
	tEnd := time.Now()
	outcome, detail := "ok", ""
	if err != nil {
		outcome, detail = "timeout", err.Error()
	} else {
		tEnd = e.T
	}
	return Row{
		Scenario: "t5", Condition: t5Condition(r.cfg.Latency, r.cfg.Service, r.cfg.Label), Run: runIdx,
		TStart: tStart, TEnd: tEnd, DurationMS: tEnd.Sub(tStart).Milliseconds(),
		Outcome: outcome, Detail: detail,
	}
}

// RunT5 executes the t5 registry-latency campaign: applies the configured
// network condition at the registry host once, runs n push-and-await
// samples under it, then restores the condition once, always, regardless
// of how the runs went. A restore failure is logged to stderr but does not
// affect the already-recorded results or the function's return value.
func RunT5(cfg T5Config, n int, out *CSVWriter) error {
	if cfg.Iface == "" {
		cfg.Iface = t5Iface
	}
	if _, _, err := t5ParseLatency(cfg.Latency); err != nil {
		return err
	}

	ctx := context.Background()
	if err := t5Apply(ctx, cfg); err != nil {
		return fmt.Errorf("apply network condition: %w", err)
	}
	defer t5Restore(ctx, cfg)

	r := newT5Runner(cfg)
	return Run(out, n, r.run)
}
