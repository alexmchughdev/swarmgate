package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/harness"
)

func init() {
	register(scenarioCmd{name: "t5", bind: bindT5})
}

func bindT5(fs *flag.FlagSet, sf *sharedFlags) func() error {
	// Not a fs.Duration flag: "unreachable" is not a valid time.Duration
	// literal, so validation is deferred to harness.RunT5 via a plain string.
	latency := fs.String("latency", "0", "registry latency: 0|500ms|5s|unreachable")
	registryHost := fs.String("registry-host", "", "ssh target for the registry host, e.g. user@registry-host (required)")
	iface := fs.String("iface", "eth0", "interface tc operates on at the registry host")
	repo := fs.String("repo", "", "path to an existing working-tree git clone the harness commits to (required)")
	stack := fs.String("stack", "t5", "stack name; file is <stack>.yaml at the repo root")
	service := fs.String("service", "web1", "service name bumped each run")
	push := fs.Bool("push", true, "push after commit; false = commit only, for local-remote setups")
	timeout := fs.Duration("timeout", 5*time.Minute, "per-run convergence timeout")
	imageRegistry := fs.String("image-registry", "", "host[:port] prefix for the pushed service's image; empty = Docker Hub, unprefixed. Distinct from --registry-host: for the 'unreachable' condition to have any effect, this should normally equal --registry-host's address, since the DROP rule only blocks that host's registry port")
	image := fs.String("image", "", "image repository name; empty = nginx (the built-in default tag cycle)")
	tags := fs.String("tags", "", "comma-separated tag cycle; empty = the built-in nginx alpine tags")

	return func() error {
		if *repo == "" {
			return fmt.Errorf("--repo is required")
		}
		if *registryHost == "" {
			return fmt.Errorf("--registry-host is required")
		}
		var tagList []string
		if *tags != "" {
			tagList = strings.Split(*tags, ",")
		}
		cfg := harness.T5Config{
			Repo: *repo, Stack: *stack, Service: *service,
			RegistryHost: *registryHost, Iface: *iface, Latency: *latency,
			Push: *push, EventsFile: sf.eventsFile, Timeout: *timeout, Label: sf.label,
			ImageRegistry: *imageRegistry, Image: *image, Tags: tagList,
		}
		out, err := harness.OpenCSVWriter(sf.out)
		if err != nil {
			return err
		}
		defer out.Close()
		return harness.RunT5(cfg, sf.n, out)
	}
}
