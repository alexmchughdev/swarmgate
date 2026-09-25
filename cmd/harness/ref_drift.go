package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/harness"
)

func init() {
	register(scenarioCmd{name: "refDrift", bind: bindRefDrift, noEventsFile: true})
}

func bindRefDrift(fs *flag.FlagSet, sf *sharedFlags) func() error {
	repo := fs.String("repo", "", "path to an existing working-tree git clone of the repo ArgoCD watches (required)")
	stack := fs.String("stack", "refConverge", "subdirectory under --repo holding the baseline manifest")
	appName := fs.String("app-name", "", "ArgoCD Application resource name, already created and pointed at --stack (required)")
	namespace := fs.String("namespace", "argocd", "namespace the ArgoCD Application resource lives in")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig for kubectl calls; empty = kubectl's own default resolution")
	drift := fs.String("drift", "", "drift kind: replicas|image|env|removed (required)")
	deployment := fs.String("deployment", "web1", "baseline deployment name (also its container name)")
	registry := fs.String("registry", "", "optional host[:port] prefix for image references; empty = Docker Hub, unprefixed")
	image := fs.String("image", "", "image repository name; empty = nginx")
	pollEvery := fs.Duration("poll-every", time.Second, "ArgoCD Application status poll interval")
	triggerRefresh := fs.Bool("trigger-refresh", false, "annotate the Application for an immediate hard refresh after the baseline push (see RefConvergeConfig.TriggerRefresh)")
	timeout := fs.Duration("timeout", 3*time.Minute, "per-phase (detect, repair) timeout")

	return func() error {
		if *repo == "" {
			return fmt.Errorf("--repo is required")
		}
		if *appName == "" {
			return fmt.Errorf("--app-name is required")
		}
		if *drift == "" {
			return fmt.Errorf("--drift is required")
		}
		cfg := harness.RefDriftConfig{
			Repo: *repo, Stack: *stack, AppName: *appName, Namespace: *namespace, Kubeconfig: *kubeconfig,
			Drift: *drift, Deployment: *deployment, Registry: *registry, Image: *image,
			PollEvery: *pollEvery, TriggerRefresh: *triggerRefresh, Timeout: *timeout, Label: sf.label,
		}
		out, err := harness.OpenCSVWriter(sf.out)
		if err != nil {
			return err
		}
		defer out.Close()
		return harness.RunRefDrift(cfg, sf.n, out)
	}
}
