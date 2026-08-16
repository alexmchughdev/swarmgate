package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/harness"
)

func init() {
	register(scenarioCmd{name: "t8", bind: bindT8, noEventsFile: true})
}

func bindT8(fs *flag.FlagSet, sf *sharedFlags) func() error {
	repo := fs.String("repo", "", "path to an existing working-tree git clone of the repo ArgoCD watches (required)")
	stack := fs.String("stack", "t8", "subdirectory under --repo holding this campaign's manifests")
	appName := fs.String("app-name", "", "ArgoCD Application resource name, already created and pointed at --stack (required)")
	namespace := fs.String("namespace", "argocd", "namespace the ArgoCD Application resource lives in")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig for kubectl calls; empty = kubectl's own default resolution")
	scale := fs.Int("scale", 1, "number of deployments in the manifest set (1|10|50)")
	changes := fs.Int("changes", 1, "number of deployments bumped per run")
	push := fs.Bool("push", true, "push after commit; false = commit only, for local-remote setups")
	timeout := fs.Duration("timeout", 3*time.Minute, "per-run convergence timeout")
	pollEvery := fs.Duration("poll-every", time.Second, "ArgoCD Application status poll interval")
	registry := fs.String("registry", "", "optional host[:port] prefix for image references; empty = Docker Hub, unprefixed")
	image := fs.String("image", "", "image repository name; empty = nginx (the built-in default tag cycle)")
	tags := fs.String("tags", "", "comma-separated tag cycle; empty = the built-in nginx alpine tags")
	triggerRefresh := fs.Bool("trigger-refresh", false, "annotate the Application for an immediate hard refresh after each push, standing in for a webhook notification (see T8Config.TriggerRefresh)")

	return func() error {
		if *repo == "" {
			return fmt.Errorf("--repo is required")
		}
		if *appName == "" {
			return fmt.Errorf("--app-name is required")
		}
		var tagList []string
		if *tags != "" {
			tagList = strings.Split(*tags, ",")
		}
		cfg := harness.T8Config{
			Repo: *repo, Stack: *stack, AppName: *appName, Namespace: *namespace, Kubeconfig: *kubeconfig,
			Scale: *scale, Changes: *changes, Push: *push,
			Registry: *registry, Image: *image, Tags: tagList,
			PollEvery: *pollEvery, TriggerRefresh: *triggerRefresh, Timeout: *timeout, Label: sf.label,
		}
		out, err := harness.OpenCSVWriter(sf.out)
		if err != nil {
			return err
		}
		defer out.Close()
		return harness.RunT8(cfg, sf.n, out)
	}
}
