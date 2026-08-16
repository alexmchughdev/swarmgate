package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// T9 is T8's T2-equivalent: drift detection and repair against ArgoCD
// instead of swarmgate. Not every T2 drift kind has an equally strong K8s
// analogue -- see docs/build-log.md's T8 drift-kind mapping table. t9DriftKinds
// covers the four with a direct native equivalent; "unmanaged" is
// deliberately not implemented here (recorded as a comparison-table row
// with a qualitative note instead of an N=30 campaign -- it tests something
// close to true-by-construction given how ArgoCD scopes "managed", not a
// live race condition worth statistically sampling).
var t9DriftKinds = map[string]bool{
	"replicas": true,
	"image":    true,
	"env":      true,
	"removed":  true,
}

// T9Config configures one t9 drift-injection campaign against ArgoCD.
type T9Config struct {
	Repo           string // path to an existing working-tree clone of the repo ArgoCD watches
	Stack          string // subdirectory under Repo holding the baseline manifest (same convention as T8Config.Stack)
	AppName        string
	Namespace      string
	Kubeconfig     string
	Drift          string // one of t9DriftKinds
	Deployment     string // baseline deployment name (also container name, matching t8DeploymentYAML's convention)
	Registry       string
	Image          string
	PollEvery      time.Duration
	TriggerRefresh bool // applies to the initial baseline push only; the drift itself is injected directly against the live cluster, not via git
	Timeout        time.Duration
	Label          string
}

func t9Condition(cfg T9Config) string {
	return fmt.Sprintf("drift=%s;deployment=%s;%s", cfg.Drift, cfg.Deployment, cfg.Label)
}

// t9EnvDriftKey/t9EnvBaselineValue are the env var t9's "env" drift kind
// modifies. Declared in the baseline manifest specifically so ArgoCD's
// diff has an existing field-value to compare against -- an *added*,
// previously-undeclared key is invisible to ArgoCD's default strategic-
// merge-based diffing (the same reason `kubectl apply` never cleans up a
// field it never owned), confirmed by direct testing; see
// docs/contentions.md, 2026-07-23. A value *change* on an already-declared
// field is what standard diffing reliably catches.
const (
	t9EnvDriftKey      = "DRIFT_BASE"
	t9EnvBaselineValue = "stable"
	t9EnvDriftValue    = "injected"
)

// t9BaselineYAML is a fixed single-deployment manifest, replicas=1,
// pinned to image tag v1 -- the known-good state every drift kind mutates
// away from and ArgoCD's selfHeal is expected to restore. The "env" drift
// kind gets one declared env var (see t9EnvDriftKey above); every other
// drift kind uses the plain t8DeploymentYAML rendering, unchanged.
func t9BaselineYAML(drift, name, image string) string {
	if drift != "env" {
		return t8DeploymentYAML([]t1Service{{Name: name, Image: image}})
	}
	base := t8DeploymentYAML([]t1Service{{Name: name, Image: image}})
	envBlock := fmt.Sprintf("          env:\n            - name: %s\n              value: %q\n", t9EnvDriftKey, t9EnvBaselineValue)
	return base + envBlock
}

// t9EnsureBaseline pushes the fixed baseline manifest and waits for ArgoCD
// to report it Synced+Healthy before any drift is injected. Idempotent:
// pushing identical content when the deployment is already at baseline is
// a no-op commit-wise (gitCommitAndPush would return an error on a
// genuinely empty diff), so this only actually pushes+waits on the very
// first call; subsequent calls between drift kinds skip straight through
// once the working tree already matches.
func t9EnsureBaseline(cfg T9Config) error {
	manifestDir := filepath.Join(cfg.Repo, cfg.Stack)
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", manifestDir, err)
	}
	image := imageRef(cfg.Registry, cfg.Image, "v1")
	manifestPath := filepath.Join(manifestDir, "deployments.yaml")
	content := t9BaselineYAML(cfg.Drift, cfg.Deployment, image)
	existing, _ := os.ReadFile(manifestPath)
	if string(existing) == content {
		return nil // already at baseline; nothing to push
	}
	if err := os.WriteFile(manifestPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write baseline manifest: %w", err)
	}
	relPath := filepath.Join(cfg.Stack, "deployments.yaml")
	sha, err := gitCommitAndPush(cfg.Repo, relPath, "t9 baseline", true)
	if err != nil {
		return fmt.Errorf("push baseline: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	if cfg.TriggerRefresh {
		_ = t8TriggerRefresh(ctx, cfg.Kubeconfig, cfg.Namespace, cfg.AppName)
	}
	return t8AwaitConverged(ctx, cfg.Kubeconfig, cfg.Namespace, cfg.AppName, sha, cfg.PollEvery)
}

func t9Kubectl(ctx context.Context, kubeconfig string, args ...string) error {
	if kubeconfig != "" {
		args = append([]string{"--kubeconfig", kubeconfig}, args...)
	}
	out, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl %v: %w: %s", args, err, out)
	}
	return nil
}

// t9Mutate injects one out-of-band drift directly against the live
// cluster, bypassing git entirely -- the K8s-side equivalent of t2Mutate's
// direct Docker API calls.
func t9Mutate(ctx context.Context, cfg T9Config) error {
	name := cfg.Deployment
	switch cfg.Drift {
	case "replicas":
		return t9Kubectl(ctx, cfg.Kubeconfig, "scale", "deployment/"+name, "--replicas=3")
	case "image":
		rollback := imageRef(cfg.Registry, cfg.Image, "v2")
		return t9Kubectl(ctx, cfg.Kubeconfig, "set", "image", "deployment/"+name, name+"="+rollback)
	case "env":
		// Changes t9EnvDriftKey's already-declared value, rather than
		// adding a new undeclared key: ArgoCD's default diff doesn't
		// see an added-but-never-desired field as drift at all (see
		// docs/contentions.md, 2026-07-23), but a value change on a
		// field the desired manifest *does* declare is caught normally.
		return t9Kubectl(ctx, cfg.Kubeconfig, "set", "env", "deployment/"+name, t9EnvDriftKey+"="+t9EnvDriftValue)
	case "removed":
		return t9Kubectl(ctx, cfg.Kubeconfig, "delete", "deployment/"+name)
	default:
		return fmt.Errorf("t9: unknown drift kind %q", cfg.Drift)
	}
}

// t9AwaitSyncStatus polls the Application until its sync.status equals want
// OR its health.status has moved off "Healthy", or ctx is done.
//
// Sync.status alone is not a safe detect-phase signal: when selfHeal is
// enabled, ArgoCD can compare, auto-sync, and patch a cheap spec-only change
// (e.g. an env var) back to "Synced" in well under one second -- faster than
// this function's own poll interval -- so a 1s-granularity poller watching
// only for "OutOfSync" can step over the entire transient window and then
// wait indefinitely for a state that will never reappear, since the app is
// already durably Synced again. Confirmed live (rke2-server controller
// logs, 2026-07-23): "Synced -> OutOfSync" and "Healthy -> Progressing" are
// logged in the same reconciliation tick, but health.status stays
// "Progressing" for the entire real repair duration (the ~120s selfHeal
// floor), giving a poll window that can't be missed. See
// docs/contentions.md, 2026-07-23.
func t9AwaitSyncStatus(ctx context.Context, kubeconfig, namespace, appName, want string, pollEvery time.Duration) error {
	if pollEvery <= 0 {
		pollEvery = time.Second
	}
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		st, err := t8FetchAppStatus(ctx, kubeconfig, namespace, appName)
		if err == nil && (st.Status.Sync.Status == want || st.Status.Health.Status != "Healthy") {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("await sync status %q: %w", want, ctx.Err())
		}
	}
}

func t9Row(cfg T9Config, run int, phase string, tStart, tEnd time.Time, err error) Row {
	outcome, detail := "ok", phase
	if err != nil {
		outcome, detail = "timeout", phase+";"+err.Error()
	}
	return Row{
		Scenario: "t9", Condition: t9Condition(cfg), Run: run,
		TStart: tStart, TEnd: tEnd, DurationMS: tEnd.Sub(tStart).Milliseconds(),
		Outcome: outcome, Detail: detail,
	}
}

func t9ErrorRow(cfg T9Config, run int, phase string, err error) Row {
	now := time.Now()
	return Row{
		Scenario: "t9", Condition: t9Condition(cfg), Run: run,
		TStart: now, TEnd: now, DurationMS: 0,
		Outcome: "error", Detail: phase + ";" + err.Error(),
	}
}

// t9RunOne executes one detect+repair cycle: ensure baseline, inject
// drift, wait for OutOfSync (detect), wait for Synced+Healthy again
// (repair, via ArgoCD's own selfHeal -- already enabled on the t8/t9
// Application, same as swarmgate's reconcile loop needs no separate
// trigger to correct drift once noticed). Two rows per run, mirroring
// t2Run's own detect/repair row pair exactly.
func t9RunOne(cfg T9Config, run int, out *CSVWriter) error {
	if err := t9EnsureBaseline(cfg); err != nil {
		return out.Write(t9ErrorRow(cfg, run, "baseline", err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	tStart := time.Now()
	if err := t9Mutate(ctx, cfg); err != nil {
		return out.Write(t9ErrorRow(cfg, run, "mutate", err))
	}
	// TriggerRefresh here (not just in t9EnsureBaseline) is what makes
	// this comparable to T2's events.wake dimension: without it, ArgoCD
	// only notices the drift on its own background reconciliation cache
	// cycle (this cluster's default: ~2 minutes), the direct analogue of
	// swarmgate's poll-only path -- not a "backoff" the way an earlier,
	// now-corrected diagnosis in docs/contentions.md described it. With
	// it, detection is immediate, the analogue of events.wake=on / a
	// real webhook.
	if cfg.TriggerRefresh {
		_ = t8TriggerRefresh(ctx, cfg.Kubeconfig, cfg.Namespace, cfg.AppName)
	}

	detectErr := t9AwaitSyncStatus(ctx, cfg.Kubeconfig, cfg.Namespace, cfg.AppName, "OutOfSync", cfg.PollEvery)
	tDetect := time.Now()
	detectRow := t9Row(cfg, run, "detect", tStart, tDetect, detectErr)
	if err := out.Write(detectRow); err != nil {
		return err
	}
	if detectErr != nil {
		return nil
	}

	repairErr := t8AwaitConvergedHealthOnly(ctx, cfg.Kubeconfig, cfg.Namespace, cfg.AppName, cfg.PollEvery)
	tRepair := time.Now()
	repairRow := t9Row(cfg, run, "repair", tDetect, tRepair, repairErr)
	return out.Write(repairRow)
}

// t8AwaitConvergedHealthOnly is t8AwaitConverged without the revision
// check: the repair phase isn't waiting on a new commit (the drift never
// touched git), just on ArgoCD's own selfHeal bringing live state back to
// Synced+Healthy against the revision already deployed.
func t8AwaitConvergedHealthOnly(ctx context.Context, kubeconfig, namespace, appName string, pollEvery time.Duration) error {
	if pollEvery <= 0 {
		pollEvery = time.Second
	}
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		st, err := t8FetchAppStatus(ctx, kubeconfig, namespace, appName)
		if err == nil && st.Status.Sync.Status == "Synced" && st.Status.Health.Status == "Healthy" {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("await repaired: %w", ctx.Err())
		}
	}
}

// RunT9 executes the t9 ArgoCD drift-detect-and-repair campaign: n
// detect+repair cycles against a fixed baseline deployment, one drift
// kind per campaign (mirrors RunT2's own one-drift-kind-per-invocation
// shape).
func RunT9(cfg T9Config, n int, out *CSVWriter) error {
	if _, ok := t9DriftKinds[cfg.Drift]; !ok {
		return fmt.Errorf("t9: unknown drift kind %q", cfg.Drift)
	}
	for run := 1; run <= n; run++ {
		if err := t9RunOne(cfg, run, out); err != nil {
			return fmt.Errorf("run %d: %w", run, err)
		}
	}
	return nil
}
