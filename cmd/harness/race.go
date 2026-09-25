package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/harness"
)

func init() {
	register(scenarioCmd{name: "race", bind: bindRace})
}

func bindRace(fs *flag.FlagSet, sf *sharedFlags) func() error {
	offset := fs.String("offset", "", "race trigger offset: diff|window|apply|after (required)")
	service := fs.String("service", "", "compose service key within the stack to race, e.g. \"web1\" (required; not stack-qualified — the harness qualifies it internally)")
	repo := fs.String("repo", "", "path to an existing working-tree git clone the harness commits to (required)")
	stack := fs.String("stack", "race", "stack name; file is <stack>.yaml at the repo root")
	dockerHost := fs.String("docker-host", "", "docker engine host for the operator mutation and verification inspect")
	timeout := fs.Duration("timeout", 3*time.Minute, "per-run deadline for reaching a settled state (converged or timeout)")
	registry := fs.String("registry", "", "optional host[:port] prefix for image references; empty = Docker Hub, unprefixed")
	image := fs.String("image", "", "image repository name; empty = nginx (the built-in default tag cycle)")
	tags := fs.String("tags", "", "comma-separated tag cycle; empty = the built-in nginx alpine tags")

	return func() error {
		if *repo == "" {
			return fmt.Errorf("--repo is required")
		}
		if *service == "" {
			return fmt.Errorf("--service is required")
		}
		if !isValidRaceOffset(*offset) {
			return fmt.Errorf("--offset must be one of %v, got %q", harness.RaceOffsets, *offset)
		}
		var tagList []string
		if *tags != "" {
			tagList = strings.Split(*tags, ",")
		}
		cfg := harness.RaceConfig{
			Repo: *repo, Stack: *stack, Service: *service, Offset: *offset,
			DockerHost: *dockerHost, EventsFile: sf.eventsFile, Timeout: *timeout, Label: sf.label,
			Registry: *registry, Image: *image, Tags: tagList,
		}
		out, err := harness.OpenCSVWriter(sf.out)
		if err != nil {
			return err
		}
		defer out.Close()
		return harness.RunRace(cfg, sf.n, out)
	}
}

func isValidRaceOffset(offset string) bool {
	for _, o := range harness.RaceOffsets {
		if offset == o {
			return true
		}
	}
	return false
}
