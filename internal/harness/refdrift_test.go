package harness

import (
	"strings"
	"testing"
)

func TestRefdriftCondition(t *testing.T) {
	cfg := RefdriftConfig{Drift: "replicas", Deployment: "web1", Label: "drift-equivalent"}
	got := refdriftCondition(cfg)
	want := "drift=replicas;deployment=web1;drift-equivalent"
	if got != want {
		t.Fatalf("refdriftCondition = %q, want %q", got, want)
	}
}

func TestRefdriftBaselineYAMLMatchesRefconvergeRendering(t *testing.T) {
	got := refdriftBaselineYAML("replicas", "web1", "eval-workload:v1")
	for _, want := range []string{"name: web1", "image: eval-workload:v1", "replicas: 1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("baseline manifest missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "---") {
		t.Fatalf("single-deployment baseline should have no document separator:\n%s", got)
	}
	if strings.Contains(got, "env:") {
		t.Fatalf("non-env drift kind should not declare an env block:\n%s", got)
	}
}

func TestRefdriftBaselineYAMLDeclaresEnvVarForEnvDriftOnly(t *testing.T) {
	got := refdriftBaselineYAML("env", "web1", "eval-workload:v1")
	for _, want := range []string{"name: web1", "image: eval-workload:v1", "env:", refdriftEnvDriftKey, refdriftEnvBaselineValue} {
		if !strings.Contains(got, want) {
			t.Fatalf("env baseline manifest missing %q:\n%s", want, got)
		}
	}
}

func TestRefdriftDriftKindsCoverTheFourWithCleanAnalogues(t *testing.T) {
	want := []string{"replicas", "image", "env", "removed"}
	if len(refdriftDriftKinds) != len(want) {
		t.Fatalf("refdriftDriftKinds has %d entries, want %d (%v)", len(refdriftDriftKinds), len(want), want)
	}
	for _, k := range want {
		if !refdriftDriftKinds[k] {
			t.Fatalf("refdriftDriftKinds missing %q", k)
		}
	}
	if refdriftDriftKinds["unmanaged"] {
		t.Fatalf("refdriftDriftKinds should not include unmanaged -- deliberately excluded: ArgoCD scopes \"managed\" to its own Application-tracked resources plus an auto-applied ownership label, not an always-on scan the way swarmgate's swarmgate.managed=true label works, so this drift kind has no equally meaningful ArgoCD analogue")
	}
}

func TestRunRefdriftRejectsUnknownDriftKind(t *testing.T) {
	err := RunRefdrift(RefdriftConfig{Drift: "bogus"}, 1, nil)
	if err == nil {
		t.Fatal("expected an error for an unknown drift kind")
	}
}
