package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/spec"
)

const t3FixtureYAML = `
manager:
  kill: "ssh root@pve qm stop 101"
  restore: "ssh root@pve qm start 101"
worker1:
  kill: "ssh root@pve qm stop 102"
  restore: "ssh root@pve qm start 102"
reconciler:
  kill: "ssh alpine1 rc-service swarmgate stop"
  restore: "ssh alpine1 rc-service swarmgate start"
`

func TestLoadHostsConfigParsesExampleShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(path, []byte(t3FixtureYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadHostsConfig(path)
	if err != nil {
		t.Fatalf("LoadHostsConfig: %v", err)
	}
	want := hostsConfig{
		"manager":    {Kill: "ssh root@pve qm stop 101", Restore: "ssh root@pve qm start 101"},
		"worker1":    {Kill: "ssh root@pve qm stop 102", Restore: "ssh root@pve qm start 102"},
		"reconciler": {Kill: "ssh alpine1 rc-service swarmgate stop", Restore: "ssh alpine1 rc-service swarmgate start"},
	}
	if len(got) != len(want) {
		t.Fatalf("loaded %d roles, want %d: %+v", len(got), len(want), got)
	}
	for role, action := range want {
		if got[role] != action {
			t.Fatalf("role %q = %+v, want %+v", role, got[role], action)
		}
	}
}

func TestLoadHostsConfigMissingFile(t *testing.T) {
	if _, err := LoadHostsConfig(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("want error for missing hosts config file")
	}
}

func TestLoadHostsConfigRepoExample(t *testing.T) {
	// The example file at the repo root must itself parse cleanly and
	// contain at least the three roles used elsewhere in this task's docs.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(wd, "..", "..", "harness-hosts.example.yaml")
	got, err := LoadHostsConfig(path)
	if err != nil {
		t.Fatalf("LoadHostsConfig(repo example): %v", err)
	}
	for _, role := range []string{"manager", "worker1", "reconciler"} {
		if _, ok := got[role]; !ok {
			t.Fatalf("repo example hosts config missing role %q: %+v", role, got)
		}
	}
}

func TestT3ResolveTargetPresent(t *testing.T) {
	hosts := hostsConfig{"worker1": {Kill: "k", Restore: "r"}}
	a, err := t3ResolveTarget(hosts, "worker1")
	if err != nil {
		t.Fatalf("t3ResolveTarget: %v", err)
	}
	if a.Kill != "k" || a.Restore != "r" {
		t.Fatalf("resolved action = %+v", a)
	}
}

func TestT3ResolveTargetAbsent(t *testing.T) {
	hosts := hostsConfig{"worker1": {Kill: "k", Restore: "r"}, "manager": {Kill: "k2", Restore: "r2"}}
	_, err := t3ResolveTarget(hosts, "worker2")
	if err == nil {
		t.Fatal("want error for target not present in hosts config")
	}
	if !strings.Contains(err.Error(), "worker2") {
		t.Fatalf("error %q does not mention the bad target", err.Error())
	}
	if !strings.Contains(err.Error(), "manager") || !strings.Contains(err.Error(), "worker1") {
		t.Fatalf("error %q does not list configured roles", err.Error())
	}
}

func TestValidateT3TargetMirrorsResolve(t *testing.T) {
	hosts := hostsConfig{"worker1": {Kill: "k", Restore: "r"}}
	if err := ValidateT3Target(hosts, "worker1"); err != nil {
		t.Fatalf("ValidateT3Target(present): %v", err)
	}
	if err := ValidateT3Target(hosts, "bogus"); err == nil {
		t.Fatal("ValidateT3Target(absent): want error")
	}
}

func TestT3Condition(t *testing.T) {
	got := t3Condition("worker1", 60*time.Second, "events=on")
	want := "target=worker1;restore_after=1m0s;events=on"
	if got != want {
		t.Fatalf("t3Condition = %q, want %q", got, want)
	}
}

func TestT3ConditionZeroRestoreAfter(t *testing.T) {
	got := t3Condition("manager", 0, "events=off")
	want := "target=manager;restore_after=0s;events=off"
	if got != want {
		t.Fatalf("t3Condition = %q, want %q", got, want)
	}
}

func TestT3FalseOKEqualSpecsConvergedIsNotFalseOK(t *testing.T) {
	s := spec.ServiceSpec{Name: "t3_web1", Image: "nginx:1.24-alpine", Replicas: 1}
	if got := t3FalseOK(s, s, true); got {
		t.Fatal("equal specs with converged=true must not be false_ok")
	}
}

func TestT3FalseOKDifferingSpecsConvergedIsFalseOK(t *testing.T) {
	desired := spec.ServiceSpec{Name: "t3_web1", Image: "nginx:1.24-alpine", Replicas: 1}
	observed := spec.ServiceSpec{Name: "t3_web1", Image: "nginx:1.25-alpine", Replicas: 1}
	if got := t3FalseOK(desired, observed, true); !got {
		t.Fatal("differing specs with converged=true must be false_ok")
	}
}

func TestT3FalseOKDifferingSpecsNotConvergedIsNotFalseOK(t *testing.T) {
	desired := spec.ServiceSpec{Name: "t3_web1", Image: "nginx:1.24-alpine", Replicas: 1}
	observed := spec.ServiceSpec{Name: "t3_web1", Image: "nginx:1.25-alpine", Replicas: 2}
	if got := t3FalseOK(desired, observed, false); got {
		t.Fatal("never-converged run must never be false_ok, regardless of mismatch")
	}
}

func TestT3RunShellRejectsEmptyCommand(t *testing.T) {
	// The empty check runs before ctx is touched, so a nil context is safe
	// here and this stays free of a live shell dependency.
	if err := t3RunShell(nil, ""); err == nil {
		t.Fatal("want error for empty command")
	}
}
