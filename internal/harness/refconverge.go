package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RefconvergeConfig configures one refconverge convergence run: an
// ArgoCD/RKE2 reference measurement, reproducing Scale's scale/changes
// matrix against a different GitOps controller entirely. Reuses
// scalePlan/scaleService/scaleServiceName unchanged (they only ever
// generated an abstract {name, image} list, never anything Swarm-specific)
// so the two scenarios' underlying condition matrix is provably identical,
// not just similarly-named.
type RefconvergeConfig struct {
	Repo       string // path to an existing working-tree clone of the repo ArgoCD watches
	Stack      string // subdirectory under Repo holding this run's manifests
	AppName    string // ArgoCD Application resource name (assumed already created, pointed at Stack)
	Namespace  string // ArgoCD's own namespace (where the Application resource lives), not the deployed workload's
	Kubeconfig string // path to the kubeconfig used for kubectl calls
	Scale      int
	Changes    int
	Push       bool
	Registry   string
	Image      string
	Tags       []string
	PollEvery  time.Duration // ArgoCD Application status poll interval; 0 defaults to 1s
	// TriggerRefresh requests a hard refresh (argocd.argoproj.io/refresh=hard
	// annotation) immediately after each push, standing in for a webhook
	// notification no git-server-side webhook integration was set up to
	// provide. Without it, ArgoCD only notices a new commit on its own
	// periodic reconciliation timer (this cluster's default: 180s) --
	// correct for measuring the poll-only case, but impractical for a
	// 240-run series (worst case ~12 hours of pure poll-wait). Measures
	// ArgoCD's sync+converge time given prompt notification (the realistic
	// production configuration, where a webhook is standard practice), not
	// the poll-only worst case.
	TriggerRefresh bool
	Timeout        time.Duration
	Label          string
}

func refconvergeCondition(cfg RefconvergeConfig) string {
	return fmt.Sprintf("scale=%d;changes=%d;%s", cfg.Scale, cfg.Changes, cfg.Label)
}

// refconvergeReadinessProbe mirrors evalWorkloadHealthcheck's (scale.go) probe contract
// as closely as Kubernetes' readinessProbe semantics allow: PROBE_INTERVAL
// (2s) -> periodSeconds, PROBE_TIMEOUT (2s) -> timeoutSeconds, PROBE_RETRIES
// (30) -> failureThreshold, start_period (0s) -> initialDelaySeconds (0).
//
// Docker's healthcheck runs *inside* the container (the workload image
// ships no shell, so it self-probes via `-healthcheck`, which itself just
// hits its own /health endpoint); Kubernetes' kubelet can hit the same
// /health endpoint directly over the pod network with a plain httpGet probe
// -- same 503-then-200 application-level signal (governed by the image's
// own HEALTHY_AFTER timer on both platforms), simpler transport, no image
// change needed. Without this, ArgoCD's Health status only requires "pod
// Running", a materially weaker bar than Swarm's health-gated convergence --
// a real measurement-equivalence gap between the two platforms that this
// probe closes.
const refconvergeReadinessProbe = "          readinessProbe:\n" +
	"            httpGet:\n" +
	"              path: /health\n" +
	"              port: 8080\n" +
	"            initialDelaySeconds: 0\n" +
	"            periodSeconds: 2\n" +
	"            timeoutSeconds: 2\n" +
	"            failureThreshold: 30\n" +
	"            successThreshold: 1\n"

// refconvergeDeploymentYAML renders services as one multi-document Deployment
// manifest, one document per service, matching scaleStackYAML's shape
// (scale independent single-replica units, changes of them bumped per
// run) in Kubernetes' own resource form instead of a compose stack.
func refconvergeDeploymentYAML(services []scaleService) string {
	var b strings.Builder
	for i, s := range services {
		if i > 0 {
			b.WriteString("---\n")
		}
		fmt.Fprintf(&b, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  labels:
    app: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
    spec:
      containers:
        - name: %s
          image: %s
%s`, s.Name, s.Name, s.Name, s.Name, s.Name, s.Image, refconvergeReadinessProbe)
	}
	return b.String()
}

// argoAppStatus is the slice of `kubectl get application ... -o json` this
// package actually reads. ArgoCD's own Application CRD carries much more;
// only sync/health/revision are needed to decide convergence.
type argoAppStatus struct {
	Status struct {
		Sync struct {
			Status   string `json:"status"`
			Revision string `json:"revision"`
		} `json:"sync"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
	} `json:"status"`
}

// refconvergeFetchAppStatus shells out to kubectl rather than a typed client:
// Application is a CRD swarmgate has no generated clientset for, and every
// other cross-process call in this harness (docker, ssh) already goes
// through os/exec — a raw dynamic-client dependency for one JSON read
// would be more machinery than the read is worth.
func refconvergeFetchAppStatus(ctx context.Context, kubeconfig, namespace, appName string) (argoAppStatus, error) {
	args := []string{"get", "application", appName, "-n", namespace, "-o", "json"}
	if kubeconfig != "" {
		args = append([]string{"--kubeconfig", kubeconfig}, args...)
	}
	out, err := exec.CommandContext(ctx, "kubectl", args...).Output()
	if err != nil {
		return argoAppStatus{}, fmt.Errorf("kubectl get application %s: %w", appName, err)
	}
	var st argoAppStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return argoAppStatus{}, fmt.Errorf("parse application status: %w", err)
	}
	return st, nil
}

// refconvergeTriggerRefresh annotates the Application to request an immediate hard
// refresh, the stand-in for a webhook notification described on
// RefconvergeConfig.TriggerRefresh. Best-effort: a failure here just means this run
// falls back to waiting out ArgoCD's own poll interval, not a run-ending
// error -- the harness's own AwaitConverged loop is what actually decides
// pass/fail.
func refconvergeTriggerRefresh(ctx context.Context, kubeconfig, namespace, appName string) error {
	args := []string{"annotate", "application", appName, "-n", namespace, "argocd.argoproj.io/refresh=hard", "--overwrite"}
	if kubeconfig != "" {
		args = append([]string{"--kubeconfig", kubeconfig}, args...)
	}
	return exec.CommandContext(ctx, "kubectl", args...).Run()
}

// refconvergeAwaitConverged polls the ArgoCD Application's own status (an
// external observer's view via its status API — matching how swarmgate's
// own convergence is measured, never ArgoCD's internal reconciliation
// timestamps) until it reports the given commit sha fully synced and
// healthy, or ctx is done.
func refconvergeAwaitConverged(ctx context.Context, kubeconfig, namespace, appName, sha string, pollEvery time.Duration) error {
	if pollEvery <= 0 {
		pollEvery = time.Second
	}
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		st, err := refconvergeFetchAppStatus(ctx, kubeconfig, namespace, appName)
		if err == nil && st.Status.Sync.Revision == sha &&
			st.Status.Sync.Status == "Synced" && st.Status.Health.Status == "Healthy" {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("await converged: %w", ctx.Err())
		}
	}
}

type refconvergeRunner struct {
	cfg      RefconvergeConfig
	tagIndex []int
}

func newRefconvergeRunner(cfg RefconvergeConfig) *refconvergeRunner {
	return &refconvergeRunner{cfg: cfg}
}

func (r *refconvergeRunner) run(runIdx int) Row {
	services, next := scalePlan(r.cfg.Scale, r.cfg.Changes, r.tagIndex, r.cfg.Registry, r.cfg.Image, r.cfg.Tags, false)
	r.tagIndex = next

	manifestDir := filepath.Join(r.cfg.Repo, r.cfg.Stack)
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		return refconvergeErrorRow(runIdx, r.cfg, fmt.Errorf("mkdir %s: %w", manifestDir, err))
	}
	manifestPath := filepath.Join(manifestDir, "deployments.yaml")
	if err := os.WriteFile(manifestPath, []byte(refconvergeDeploymentYAML(services)), 0o644); err != nil {
		return refconvergeErrorRow(runIdx, r.cfg, fmt.Errorf("write manifest: %w", err))
	}

	relPath := filepath.Join(r.cfg.Stack, "deployments.yaml")
	sha, err := gitCommitAndPush(r.cfg.Repo, relPath, fmt.Sprintf("refconverge run %d", runIdx), r.cfg.Push)
	if err != nil {
		return refconvergeErrorRow(runIdx, r.cfg, err)
	}

	tStart := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.Timeout)
	defer cancel()
	if r.cfg.TriggerRefresh {
		_ = refconvergeTriggerRefresh(ctx, r.cfg.Kubeconfig, r.cfg.Namespace, r.cfg.AppName)
	}
	err = refconvergeAwaitConverged(ctx, r.cfg.Kubeconfig, r.cfg.Namespace, r.cfg.AppName, sha, r.cfg.PollEvery)
	tEnd := time.Now()
	outcome, detail := "ok", ""
	if err != nil {
		outcome, detail = "timeout", err.Error()
	}
	return Row{
		Scenario: "refconverge", Condition: refconvergeCondition(r.cfg), Run: runIdx,
		TStart: tStart, TEnd: tEnd, DurationMS: tEnd.Sub(tStart).Milliseconds(),
		Outcome: outcome, Detail: detail,
	}
}

func refconvergeErrorRow(runIdx int, cfg RefconvergeConfig, err error) Row {
	now := time.Now()
	return Row{
		Scenario: "refconverge", Condition: refconvergeCondition(cfg), Run: runIdx,
		TStart: now, TEnd: now, DurationMS: 0, Outcome: "error", Detail: err.Error(),
	}
}

// RunRefconverge executes an ArgoCD-reference convergence run: n
// iterations, each generating a scale-deployment manifest set, bumping changes
// deployments' image tags, committing (and optionally pushing) to the
// repo ArgoCD watches, and measuring time to the matching Application
// sync+health state — the same scale/changes matrix RunScale exercises
// against swarmgate, against ArgoCD instead.
func RunRefconverge(cfg RefconvergeConfig, n int, out *CSVWriter) error {
	r := newRefconvergeRunner(cfg)
	return Run(out, n, r.run)
}
