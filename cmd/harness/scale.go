package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/harness"
)

func init() {
	register(scenarioCmd{name: "scale", bind: bindScale})
}

func bindScale(fs *flag.FlagSet, sf *sharedFlags) func() error {
	repo := fs.String("repo", "", "path to an existing working-tree git clone the harness commits to (required)")
	stack := fs.String("stack", "scale", "stack name; file is <stack>.yaml at the repo root")
	scale := fs.Int("scale", 1, "number of services in the stack (1|10|50)")
	changes := fs.Int("changes", 1, "number of services bumped per run")
	push := fs.Bool("push", true, "push after commit; false = commit only, for local-remote setups")
	timeout := fs.Duration("timeout", 3*time.Minute, "per-run convergence timeout")
	registry := fs.String("registry", "", "optional host[:port] prefix for image references; empty = Docker Hub, unprefixed")
	image := fs.String("image", "", "image repository name; empty = nginx (the built-in default tag cycle)")
	tags := fs.String("tags", "", "comma-separated tag cycle; empty = the built-in nginx alpine tags")
	healthcheck := fs.Bool("healthcheck", false, "emit the eval-workload image's healthcheck block on every generated service (see evalWorkloadHealthcheck)")

	return func() error {
		if *repo == "" {
			return fmt.Errorf("--repo is required")
		}
		var tagList []string
		if *tags != "" {
			tagList = strings.Split(*tags, ",")
		}
		cfg := harness.ScaleConfig{
			Repo: *repo, Stack: *stack, Scale: *scale, Changes: *changes,
			Push: *push, Registry: *registry, Image: *image, Tags: tagList, Healthcheck: *healthcheck,
			EventsFile: sf.eventsFile, Timeout: *timeout, Label: sf.label,
		}
		out, err := harness.OpenCSVWriter(sf.out)
		if err != nil {
			return err
		}
		defer out.Close()
		return harness.RunScale(cfg, sf.n, out)
	}
}
