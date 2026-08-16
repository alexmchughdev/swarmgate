package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/harness"
)

func init() {
	register(scenarioCmd{name: "t2", bind: bindT2})
}

// t2DriftEnum is the accepted --drift values, checked verbatim against
// internal/loop/drift.go's kind vocabulary.
var t2DriftEnum = map[string]bool{
	"replicas":  true,
	"image":     true,
	"env":       true,
	"removed":   true,
	"unmanaged": true,
}

func bindT2(fs *flag.FlagSet, sf *sharedFlags) func() error {
	drift := fs.String("drift", "", "drift kind: replicas|image|env|removed|unmanaged (required)")
	service := fs.String("service", "", "full qualified target service name; for --drift unmanaged, the decoy's base name (required)")
	pollInterval := fs.Duration("poll-interval", 30*time.Second, "must match the operator's configured swarmgate.yaml poll_interval; used only for the unmanaged case's wait window")
	dockerHost := fs.String("docker-host", "", "docker host; empty = default Docker environment")
	timeout := fs.Duration("timeout", 3*time.Minute, "per-phase detect/repair timeout")
	registry := fs.String("registry", "", "optional host[:port] prefix for the image drift/unmanaged cases' rollback image; empty = Docker Hub, unprefixed")

	return func() error {
		if !t2DriftEnum[*drift] {
			return fmt.Errorf("--drift must be one of replicas|image|env|removed|unmanaged, got %q", *drift)
		}
		if *service == "" {
			return fmt.Errorf("--service is required")
		}
		cfg := harness.T2Config{
			Drift: *drift, Service: *service, PollInterval: *pollInterval, Registry: *registry,
			EventsFile: sf.eventsFile, Timeout: *timeout, Label: sf.label,
		}
		cli, err := harness.NewT2DockerClient(*dockerHost)
		if err != nil {
			return err
		}
		out, err := harness.OpenCSVWriter(sf.out)
		if err != nil {
			return err
		}
		defer out.Close()
		return harness.RunT2(context.Background(), cfg, cli, sf.n, out)
	}
}
