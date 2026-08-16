package harness

import (
	"strings"
	"testing"
)

func TestT9Condition(t *testing.T) {
	cfg := T9Config{Drift: "replicas", Deployment: "web1", Label: "t2-equivalent"}
	got := t9Condition(cfg)
	want := "drift=replicas;deployment=web1;t2-equivalent"
	if got != want {
		t.Fatalf("t9Condition = %q, want %q", got, want)
	}
}

func TestT9BaselineYAMLMatchesT8Rendering(t *testing.T) {
	got := t9BaselineYAML("replicas", "web1", "eval-workload:v1")
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

func TestT9BaselineYAMLDeclaresEnvVarForEnvDriftOnly(t *testing.T) {
	got := t9BaselineYAML("env", "web1", "eval-workload:v1")
	for _, want := range []string{"name: web1", "image: eval-workload:v1", "env:", t9EnvDriftKey, t9EnvBaselineValue} {
		if !strings.Contains(got, want) {
			t.Fatalf("env baseline manifest missing %q:\n%s", want, got)
		}
	}
}

func TestT9DriftKindsCoverTheFourWithCleanAnalogues(t *testing.T) {
	want := []string{"replicas", "image", "env", "removed"}
	if len(t9DriftKinds) != len(want) {
		t.Fatalf("t9DriftKinds has %d entries, want %d (%v)", len(t9DriftKinds), len(want), want)
	}
	for _, k := range want {
		if !t9DriftKinds[k] {
			t.Fatalf("t9DriftKinds missing %q", k)
		}
	}
	if t9DriftKinds["unmanaged"] {
		t.Fatalf("t9DriftKinds should not include unmanaged -- deliberately excluded, see docs/build-log.md")
	}
}

func TestRunT9RejectsUnknownDriftKind(t *testing.T) {
	err := RunT9(T9Config{Drift: "bogus"}, 1, nil)
	if err == nil {
		t.Fatal("expected an error for an unknown drift kind")
	}
}
