package harness

import (
	"strings"
	"testing"
)

func TestRefDriftCondition(t *testing.T) {
	cfg := RefDriftConfig{Drift: "replicas", Deployment: "web1", Label: "drift-equivalent"}
	got := refDriftCondition(cfg)
	want := "drift=replicas;deployment=web1;drift-equivalent"
	if got != want {
		t.Fatalf("refDriftCondition = %q, want %q", got, want)
	}
}

func TestRefDriftBaselineYAMLMatchesRefConvergeRendering(t *testing.T) {
	got := refDriftBaselineYAML("replicas", "web1", "eval-workload:v1")
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

func TestRefDriftBaselineYAMLDeclaresEnvVarForEnvDriftOnly(t *testing.T) {
	got := refDriftBaselineYAML("env", "web1", "eval-workload:v1")
	for _, want := range []string{"name: web1", "image: eval-workload:v1", "env:", refDriftEnvDriftKey, refDriftEnvBaselineValue} {
		if !strings.Contains(got, want) {
			t.Fatalf("env baseline manifest missing %q:\n%s", want, got)
		}
	}
}

func TestRefDriftDriftKindsCoverTheFourWithCleanAnalogues(t *testing.T) {
	want := []string{"replicas", "image", "env", "removed"}
	if len(refDriftDriftKinds) != len(want) {
		t.Fatalf("refDriftDriftKinds has %d entries, want %d (%v)", len(refDriftDriftKinds), len(want), want)
	}
	for _, k := range want {
		if !refDriftDriftKinds[k] {
			t.Fatalf("refDriftDriftKinds missing %q", k)
		}
	}
	if refDriftDriftKinds["unmanaged"] {
		t.Fatalf("refDriftDriftKinds should not include unmanaged -- deliberately excluded: ArgoCD scopes \"managed\" to its own Application-tracked resources plus an auto-applied ownership label, not an always-on scan the way swarmgate's swarmgate.managed=true label works, so this drift kind has no equally meaningful ArgoCD analogue")
	}
}

func TestRunRefDriftRejectsUnknownDriftKind(t *testing.T) {
	err := RunRefDrift(RefDriftConfig{Drift: "bogus"}, 1, nil)
	if err == nil {
		t.Fatal("expected an error for an unknown drift kind")
	}
}
