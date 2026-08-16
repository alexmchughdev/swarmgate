package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/alexmchughdev/swarmgate/internal/harness"
)

func init() {
	register(scenarioCmd{name: "t3", bind: bindT3})
}

func bindT3(fs *flag.FlagSet, sf *sharedFlags) func() error {
	hostsConfigPath := fs.String("hosts-config", "", "path to a YAML file mapping role -> {kill, restore} shell commands (required)")
	target := fs.String("target", "", "role to fault, e.g. worker1|manager|reconciler (must be a key in --hosts-config; required)")
	restoreAfter := fs.Duration("restore-after", 0, "restore the target after this long regardless of convergence; 0 = restore once at scenario end only")
	repo := fs.String("repo", "", "path to an existing working-tree git clone the harness commits to (required)")
	stack := fs.String("stack", "t3", "stack name; file is <stack>.yaml at the repo root")
	// Default matches t1ServiceName(0): this scenario always pushes exactly
	// one service, so there is no scale/changes flag to derive it from.
	service := fs.String("service", "web1", "compose service key within the stack")
	dockerHost := fs.String("docker-host", "", "docker engine host for the post-converged verification inspect; empty = environment/socket default, ssh://user@host also supported")
	registry := fs.String("registry", "", "optional host[:port] prefix for image references; empty = Docker Hub, unprefixed")
	image := fs.String("image", "", "image repository name; empty = nginx (the built-in default tag cycle)")
	tags := fs.String("tags", "", "comma-separated tag cycle; empty = the built-in nginx alpine tags")
	pollReadout := fs.Bool("poll-readout", false, "poll live state for convergence instead of waiting on the events stream; always on for --target reconciler regardless of this flag. Set it for other targets whose events file is itself relayed across the same fault being injected (e.g. a manager-kill run observed from another host)")

	return func() error {
		if *hostsConfigPath == "" {
			return fmt.Errorf("--hosts-config is required")
		}
		if *target == "" {
			return fmt.Errorf("--target is required")
		}
		if *repo == "" {
			return fmt.Errorf("--repo is required")
		}

		hosts, err := harness.LoadHostsConfig(*hostsConfigPath)
		if err != nil {
			return err
		}
		if err := harness.ValidateT3Target(hosts, *target); err != nil {
			return err
		}

		var tagList []string
		if *tags != "" {
			tagList = strings.Split(*tags, ",")
		}
		cfg := harness.T3Config{
			Repo: *repo, Stack: *stack, Service: *service,
			Hosts: hosts, Target: *target, RestoreAfter: *restoreAfter,
			EventsFile: sf.eventsFile, DockerHost: *dockerHost, Label: sf.label,
			Registry: *registry, Image: *image, Tags: tagList,
			PollReadout: *pollReadout,
		}
		out, err := harness.OpenCSVWriter(sf.out)
		if err != nil {
			return err
		}
		defer out.Close()
		return harness.RunT3(cfg, sf.n, out)
	}
}
