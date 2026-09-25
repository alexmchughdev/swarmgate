package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/config"
	"github.com/alexmchughdev/swarmgate/internal/diff"
	"github.com/alexmchughdev/swarmgate/internal/observe"
	"github.com/alexmchughdev/swarmgate/internal/source"
	"github.com/alexmchughdev/swarmgate/internal/spec"
	"github.com/alexmchughdev/swarmgate/internal/telemetry"
)

// serviceStatus is one row of `swarmgate status`.
type serviceStatus struct {
	Name          string    `json:"name"`
	State         string    `json:"state"` // converged|diverged|error
	Digest        string    `json:"digest,omitempty"`
	LastReconcile time.Time `json:"last_reconcile"`
	Commit        string    `json:"commit"`
}

// runStatus implements `swarmgate status --config ...`. State is derived
// from a fresh poll+parse+resolve+observe+diff against the live cluster
// and current git HEAD — the same read path the reconcile loop's own
// cycle uses up through the diff stage, just without ever calling the
// gate, applier, or converger — cross-referenced with the telemetry
// JSONL's most recent apply/verify outcome per service, since a live diff
// alone can't distinguish "diverged, not yet reconciled" from "diverged,
// and reconciliation keeps failing or being rejected."
func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to swarmgate.yaml (required)")
	asJSON := fs.Bool("json", false, "output as JSON instead of a plain text table")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "swarmgate status: --config is required")
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "swarmgate status: configuration invalid:", err)
		return 2
	}

	ctx := context.Background()

	src := source.NewGitSource(cfg.Git.URL, cfg.Git.Branch, cfg.Git.Path, cfg.Git.SSHKeyFile)
	commit, files, err := src.Fetch(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "swarmgate status: fetch source:", err)
		return 1
	}
	desired, err := spec.Parse(files, cfg.Git.InterpolationVars, cfg.EnvFileRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "swarmgate status: parse:", err)
		return 1
	}
	if err := spec.Resolve(ctx, &desired, cfg.Registry.AuthFile); err != nil {
		fmt.Fprintln(os.Stderr, "swarmgate status: resolve:", err)
		return 1
	}

	observer, err := observe.NewSwarmObserver(cfg.Docker.Host)
	if err != nil {
		fmt.Fprintln(os.Stderr, "swarmgate status: create observer:", err)
		return 1
	}
	observed, err := observer.Snapshot(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "swarmgate status: observe:", err)
		return 1
	}

	lastEvent, errored, err := scanTelemetry(cfg.Telemetry.Out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "swarmgate status: read telemetry:", err)
		return 1
	}

	statuses := buildStatuses(desired, observed, errored, commit.SHA, lastEvent)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(statuses); err != nil {
			fmt.Fprintln(os.Stderr, "swarmgate status: encode json:", err)
			return 1
		}
		return 0
	}
	printStatusTable(statuses)
	return 0
}

// buildStatuses computes one row per service named in either desired or
// observed state (a service can appear in only one — a pending create or
// an unmanaged/orphaned removal), classifying it converged/diverged/error.
func buildStatuses(desired spec.DesiredState, observed spec.ObservedState, errored map[string]bool, commitSHA string, lastEvent time.Time) []serviceStatus {
	d := diff.Compute(desired, observed)
	diverged := make(map[string]bool, len(d.Creates)+len(d.Updates)+len(d.Removes))
	for _, s := range d.Creates {
		diverged[s.Name] = true
	}
	for _, u := range d.Updates {
		diverged[u.Name] = true
	}
	for _, s := range d.Removes {
		diverged[s.Name] = true
	}

	names := make(map[string]bool, len(desired.Services)+len(observed.Services))
	for name := range desired.Services {
		names[name] = true
	}
	for name := range observed.Services {
		names[name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	statuses := make([]serviceStatus, 0, len(sorted))
	for _, name := range sorted {
		state := "converged"
		if diverged[name] {
			state = "diverged"
		}
		if errored[name] {
			state = "error"
		}
		digest := ""
		if svc, ok := observed.Services[name]; ok {
			digest = shortDigest(svc.Image)
		}
		statuses = append(statuses, serviceStatus{
			Name: name, State: state, Digest: digest,
			LastReconcile: lastEvent, Commit: commitSHA,
		})
	}
	return statuses
}

// shortDigest renders the first 12 hex characters of image's digest
// (engine-familiar repo@sha256:... form), matching the truncated-ID
// convention `docker` CLI output uses.
func shortDigest(image string) string {
	_, digest, ok := strings.Cut(image, "@sha256:")
	if !ok {
		return ""
	}
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

// scanTelemetry reads path once, returning the timestamp of the last
// event seen (an approximation of "when did the reconciler last do
// anything") and, per service, whether its most recently observed
// verify/apply outcome was a reject/error. A later event for the same
// service always overwrites an earlier one, so the result reflects the
// current state, not history. A missing file (no cycle has run yet)
// is not an error.
func scanTelemetry(path string) (time.Time, map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, map[string]bool{}, nil
		}
		return time.Time{}, nil, err
	}
	defer f.Close()

	var lastEvent time.Time
	errored := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e telemetry.Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.T.After(lastEvent) {
			lastEvent = e.T
		}
		if e.Service == "" {
			continue
		}
		switch e.Stage {
		case telemetry.StageVerify:
			errored[e.Service] = e.Fields["outcome"] == "reject"
		case telemetry.StageApply:
			errored[e.Service] = e.Fields["outcome"] == "error"
		}
	}
	return lastEvent, errored, sc.Err()
}

func printStatusTable(statuses []serviceStatus) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tDIGEST\tLAST_RECONCILE\tCOMMIT")
	for _, s := range statuses {
		last := "-"
		if !s.LastReconcile.IsZero() {
			last = s.LastReconcile.UTC().Format(time.RFC3339)
		}
		digest := s.Digest
		if digest == "" {
			digest = "-"
		}
		commit := s.Commit
		if commit == "" {
			commit = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.State, digest, last, commit)
	}
	w.Flush()
}
