// Command swarmgate reconciles Docker Swarm services against a Git source of truth.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/apply"
	"github.com/alexmchughdev/swarmgate/internal/config"
	"github.com/alexmchughdev/swarmgate/internal/gate"
	"github.com/alexmchughdev/swarmgate/internal/loop"
	"github.com/alexmchughdev/swarmgate/internal/observe"
	"github.com/alexmchughdev/swarmgate/internal/source"
	"github.com/alexmchughdev/swarmgate/internal/spec"
	"github.com/alexmchughdev/swarmgate/internal/telemetry"
)

// version is overwritten at build time via -ldflags "-X main.version=...";
// see the Makefile's release target.
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "status":
			os.Exit(runStatus(os.Args[2:]))
		case "version":
			fmt.Println(version)
			os.Exit(0)
		}
	}
	os.Exit(run())
}

func run() int {
	var (
		cfgPath = flag.String("config", "", "path to swarmgate.yaml (required)")
		once    = flag.Bool("once", false, "run a single reconcile cycle and exit")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "swarmgate: --config is required")
		flag.Usage()
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("configuration invalid", "error", err)
		return 2
	}

	// 0o600: telemetry events include image references, service names, and
	// gate-rejection reasons — operational detail with no reason to be
	// world- or group-readable on a multi-tenant host.
	out, err := os.OpenFile(cfg.Telemetry.Out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		logger.Error("open telemetry output", "error", err)
		return 2
	}
	defer out.Close()

	observer, err := observe.NewSwarmObserver(cfg.Docker.Host)
	if err != nil {
		logger.Error("create observer", "error", err)
		return 1
	}
	applier, err := apply.NewSwarmApplier(cfg.Docker.Host)
	if err != nil {
		logger.Error("create applier", "error", err)
		return 1
	}

	g, err := buildGate(cfg)
	if err != nil {
		logger.Error("configure gate", "error", err)
		return 2
	}

	deps := loop.Deps{
		Source: source.NewGitSource(cfg.Git.URL, cfg.Git.Branch, cfg.Git.Path, cfg.Git.SSHKeyFile),
		Parse: func(files []source.StackFile) (spec.DesiredState, error) {
			return spec.Parse(files, cfg.Git.InterpolationVars)
		},
		Resolve: func(ctx context.Context, d *spec.DesiredState) error {
			return spec.Resolve(ctx, d, cfg.Registry.AuthFile)
		},
		Observer:  observer,
		Applier:   applier,
		Converger: observer,
		Gate:      g,
		Events:    observer.Events,
		Recorder:  telemetry.NewJSONLRecorder(out, nil),
		Log:       logger,
		Now:       time.Now,
		After:     time.After,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		if err := loop.RunOnce(ctx, deps, cfg); err != nil {
			logger.Error("reconcile failed", "error", err)
			return 1
		}
		return 0
	}
	if err := loop.Run(ctx, deps, cfg); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("loop terminated", "error", err)
		return 1
	}
	return 0
}

// buildGate returns NoopGate when the gate is disabled, and a
// policy-loaded CosignGate otherwise. Policy and key-file errors surface
// here at startup rather than on the first reconcile cycle.
func buildGate(cfg config.Config) (gate.Gate, error) {
	if !cfg.Gate.Enabled {
		return gate.NoopGate{}, nil
	}
	p, err := gate.LoadPolicy(cfg.Gate.PolicyFile)
	if err != nil {
		return nil, fmt.Errorf("load policy %s: %w", cfg.Gate.PolicyFile, err)
	}
	g, err := gate.NewCosignGate(p, cfg.Registry.AuthFile)
	if err != nil {
		return nil, err
	}
	return g, nil
}
