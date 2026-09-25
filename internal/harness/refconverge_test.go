package harness

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRefconvergeDeploymentYAMLRendersOneDocumentPerService(t *testing.T) {
	services := []scaleService{
		{Name: "web1", Image: "nginx:1.24-alpine"},
		{Name: "web2", Image: "nginx:1.25-alpine"},
	}
	got := refconvergeDeploymentYAML(services)
	if strings.Count(got, "---\n") != 1 {
		t.Fatalf("want exactly one document separator for 2 services, got:\n%s", got)
	}
	for _, want := range []string{
		"name: web1", "image: nginx:1.24-alpine",
		"name: web2", "image: nginx:1.25-alpine",
		"replicas: 1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered manifest missing %q:\n%s", want, got)
		}
	}
}

func TestRefconvergeDeploymentYAMLIncludesReadinessProbeMatchingScaleHealthcheck(t *testing.T) {
	got := refconvergeDeploymentYAML([]scaleService{{Name: "web1", Image: "nginx:1.24-alpine"}})
	for _, want := range []string{
		"readinessProbe:",
		"path: /health",
		"port: 8080",
		"initialDelaySeconds: 0",
		"periodSeconds: 2",
		"timeoutSeconds: 2",
		"failureThreshold: 30",
		"successThreshold: 1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered manifest missing %q:\n%s", want, got)
		}
	}
	var out any
	if err := yaml.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("rendered manifest is not valid YAML: %v\n%s", err, got)
	}
}

func TestRefconvergeDeploymentYAMLSingleServiceHasNoSeparator(t *testing.T) {
	got := refconvergeDeploymentYAML([]scaleService{{Name: "web1", Image: "nginx:1.24-alpine"}})
	if strings.Contains(got, "---") {
		t.Fatalf("single-service manifest should have no document separator:\n%s", got)
	}
}

func TestRefconvergeCondition(t *testing.T) {
	cfg := RefconvergeConfig{Scale: 10, Changes: 5, Label: "events=on"}
	got := refconvergeCondition(cfg)
	want := "scale=10;changes=5;events=on"
	if got != want {
		t.Fatalf("refconvergeCondition = %q, want %q", got, want)
	}
}

func TestArgoAppStatusParsesSyncAndHealth(t *testing.T) {
	raw := `{
		"status": {
			"sync": {"status": "Synced", "revision": "abc123"},
			"health": {"status": "Healthy"}
		}
	}`
	var st argoAppStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if st.Status.Sync.Status != "Synced" || st.Status.Sync.Revision != "abc123" || st.Status.Health.Status != "Healthy" {
		t.Fatalf("parsed status = %+v, want Synced/abc123/Healthy", st)
	}
}
