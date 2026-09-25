package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/harness"
)

func init() {
	register(scenarioCmd{name: "verify", bind: bindVerify})
}

func bindVerify(fs *flag.FlagSet, sf *sharedFlags) func() error {
	caseFlag := fs.String("case", "", "gate case: unsigned|wrong-identity|no-attestation|ok|tag-repoint (required)")
	mode := fs.String("mode", "", "gate.mode the swarmgate instance under test is configured with; recorded in Condition only")
	registry := fs.String("registry", "", "registry host:port pipeline/build.sh pushed the fixture images to (required)")
	repo := fs.String("repo", "", "path to an existing working-tree git clone the harness commits to (required)")
	stack := fs.String("stack", "verify", "stack name; file is <stack>.yaml at the repo root")
	service := fs.String("service", "web1", "base service name; each run appends its own index")
	dockerHost := fs.String("docker-host", "", "docker engine host; only the tag-repoint case's post-hoc inspect needs it")
	push := fs.Bool("push", true, "push after commit; false = commit only, for local-remote setups")
	timeout := fs.Duration("timeout", 2*time.Minute, "per-run deadline for reaching a settled state")

	return func() error {
		if *repo == "" {
			return fmt.Errorf("--repo is required")
		}
		if *registry == "" {
			return fmt.Errorf("--registry is required")
		}
		if !isValidVerifyCase(*caseFlag) {
			return fmt.Errorf("--case must be one of %v, got %q", harness.VerifyCases, *caseFlag)
		}
		cfg := harness.VerifyConfig{
			Repo: *repo, Stack: *stack, Service: *service, Registry: *registry,
			Case: *caseFlag, Mode: *mode, DockerHost: *dockerHost,
			Push: *push, EventsFile: sf.eventsFile, Timeout: *timeout, Label: sf.label,
		}
		out, err := harness.OpenCSVWriter(sf.out)
		if err != nil {
			return err
		}
		defer out.Close()
		return harness.RunVerify(cfg, sf.n, out)
	}
}

func isValidVerifyCase(c string) bool {
	for _, v := range harness.VerifyCases {
		if c == v {
			return true
		}
	}
	return false
}
