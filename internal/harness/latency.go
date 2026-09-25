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

// latencyIface is the default network interface tc operates on at the registry
// host; overridable via LatencyConfig.Iface.
const latencyIface = "eth0"

// latencyRegistryPort is the registry's TCP port, blocked by the "unreachable"
// condition.
const latencyRegistryPort = 5000

// LatencyConfig configures one registry-latency run.
type LatencyConfig struct {
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
	// DROP scoped to RegistryHost's port latencyRegistryPort has no effect on a
	// pull that never talks to that host): set ImageRegistry to
	// RegistryHost's own address so the pushed service actually pulls from
	// the host the network condition is being injected at.
	ImageRegistry string
	Image         string // empty defaults to "nginx"
	Tags          []string
}

// latencyParseLatency parses the --latency flag. "unreachable" selects the
// iptables DROP condition; anything else must parse as a time.Duration,
// including "0" (parses to a zero duration meaning "no injected latency" —
// the baseline/control condition, where no tc command is applied at all).
func latencyParseLatency(s string) (d time.Duration, unreachable bool, err error) {
	if s == "unreachable" {
		return 0, true, nil
	}
	d, err = time.ParseDuration(s)
	if err != nil {
		return 0, false, fmt.Errorf("invalid --latency %q: %w", s, err)
	}
	return d, false, nil
}

// latencyNetemCommand returns the tc command that injects delay on iface. delay
// is formatted with time.Duration.String, which produces unit suffixes
// (e.g. "500ms", "5s") that tc's netem qdisc also accepts for these
// magnitudes.
func latencyNetemCommand(iface string, delay time.Duration) string {
	return fmt.Sprintf("tc qdisc replace dev %s root netem delay %s", iface, delay.String())
}

// latencyNetemDeleteCommand returns the tc command that removes a netem qdisc
// previously installed by latencyNetemCommand.
func latencyNetemDeleteCommand(iface string) string {
	return fmt.Sprintf("tc qdisc del dev %s root netem", iface)
}

// latencyDropCommand returns the iptables command that drops inbound traffic to
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
func latencyDropCommand(port int) string {
	return fmt.Sprintf("iptables -I DOCKER-USER -p tcp --dport %d -j DROP", port)
}

// latencyDropDeleteCommand returns the iptables command that removes the DROP
// rule previously installed by latencyDropCommand. Same rule spec as the insert,
// with -D in place of -I.
func latencyDropDeleteCommand(port int) string {
	return fmt.Sprintf("iptables -D DOCKER-USER -p tcp --dport %d -j DROP", port)
}

// latencyCondition renders the Condition column for a latency row.
func latencyCondition(latency, service, label string) string {
	return fmt.Sprintf("latency=%s;service=%s;%s", latency, service, label)
}

// latencyRunSSH runs command on host over ssh, wrapping any output into the
// returned error so a broken ssh setup (bad host, missing tc/iptables,
// insufficient privilege) is diagnosable from the harness's own output.
func latencyRunSSH(ctx context.Context, host, command string) error {
	out, err := exec.CommandContext(ctx, "ssh", host, command).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh %s %q: %w: %s", host, command, err, out)
	}
	return nil
}

// latencyApply installs the network condition described by cfg at the registry
// host. The zero-latency case is a control/baseline condition: it applies
// nothing, so there is no tc invocation and (see latencyRestore) nothing to
// restore either.
func latencyApply(ctx context.Context, cfg LatencyConfig) error {
	d, unreachable, err := latencyParseLatency(cfg.Latency)
	if err != nil {
		return err
	}
	switch {
	case unreachable:
		return latencyRunSSH(ctx, cfg.RegistryHost, latencyDropCommand(latencyRegistryPort))
	case d == 0:
		return nil
	default:
		return latencyRunSSH(ctx, cfg.RegistryHost, latencyNetemCommand(cfg.Iface, d))
	}
}

// latencyRestore removes the network condition previously installed by latencyApply.
// Best-effort: a "tc qdisc del" on an interface with no netem qdisc
// commonly exits non-zero because there is nothing to delete, so failures
// here are logged rather than returned, and never mask run results already
// recorded.
func latencyRestore(ctx context.Context, cfg LatencyConfig) {
	_, unreachable, err := latencyParseLatency(cfg.Latency)
	if err != nil {
		// Already reported by latencyApply's failure; nothing to restore.
		return
	}
	switch {
	case unreachable:
		if err := latencyRunSSH(ctx, cfg.RegistryHost, latencyDropDeleteCommand(latencyRegistryPort)); err != nil {
			fmt.Fprintf(os.Stderr, "latency: restore (iptables) failed: %v\n", err)
		}
	default:
		d, _, _ := latencyParseLatency(cfg.Latency)
		if d == 0 {
			return // control condition: nothing was applied
		}
		if err := latencyRunSSH(ctx, cfg.RegistryHost, latencyNetemDeleteCommand(cfg.Iface)); err != nil {
			fmt.Fprintf(os.Stderr, "latency: restore (tc) failed: %v\n", err)
		}
	}
}

func latencyErrorRow(runIdx int, cfg LatencyConfig, err error) Row {
	now := time.Now()
	return Row{
		Scenario: "latency", Condition: latencyCondition(cfg.Latency, cfg.Service, cfg.Label), Run: runIdx,
		TStart: now, TEnd: now, DurationMS: 0, Outcome: "error", Detail: err.Error(),
	}
}

// latencyRunner carries cross-run state (the per-run image tag cycle) for
// one run.
type latencyRunner struct {
	cfg LatencyConfig
}

func newLatencyRunner(cfg LatencyConfig) *latencyRunner {
	return &latencyRunner{cfg: cfg}
}

func (r *latencyRunner) run(runIdx int) Row {
	tags := r.cfg.Tags
	if len(tags) == 0 {
		tags = scaleTags
	}
	imageName := r.cfg.Image
	if imageName == "" {
		imageName = "nginx"
	}
	image := imageRef(r.cfg.ImageRegistry, imageName, tags[runIdx%len(tags)])
	stackPath := filepath.Join(r.cfg.Repo, r.cfg.Stack+".yaml")
	stack := scaleStackYAML([]scaleService{{Name: r.cfg.Service, Image: image}})
	if err := os.WriteFile(stackPath, []byte(stack), 0o644); err != nil {
		return latencyErrorRow(runIdx, r.cfg, fmt.Errorf("write stack file: %w", err))
	}

	sha, err := gitCommitAndPush(r.cfg.Repo, r.cfg.Stack+".yaml", fmt.Sprintf("latency run %d", runIdx), r.cfg.Push)
	if err != nil {
		return latencyErrorRow(runIdx, r.cfg, err)
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
		Scenario: "latency", Condition: latencyCondition(r.cfg.Latency, r.cfg.Service, r.cfg.Label), Run: runIdx,
		TStart: tStart, TEnd: tEnd, DurationMS: tEnd.Sub(tStart).Milliseconds(),
		Outcome: outcome, Detail: detail,
	}
}

// RunLatency executes a registry-latency run: applies the configured
// network condition at the registry host once, runs n push-and-await
// samples under it, then restores the condition once, always, regardless
// of how the runs went. A restore failure is logged to stderr but does not
// affect the already-recorded results or the function's return value.
func RunLatency(cfg LatencyConfig, n int, out *CSVWriter) error {
	if cfg.Iface == "" {
		cfg.Iface = latencyIface
	}
	if _, _, err := latencyParseLatency(cfg.Latency); err != nil {
		return err
	}

	ctx := context.Background()
	if err := latencyApply(ctx, cfg); err != nil {
		return fmt.Errorf("apply network condition: %w", err)
	}
	defer latencyRestore(ctx, cfg)

	r := newLatencyRunner(cfg)
	return Run(out, n, r.run)
}
