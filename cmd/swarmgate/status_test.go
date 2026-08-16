package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/spec"
)

func TestShortDigest(t *testing.T) {
	tests := []struct {
		image string
		want  string
	}{
		{"nginx@sha256:abcdef012345678901234567890", "abcdef012345"},
		{"nginx@sha256:abc", "abc"},
		{"nginx:latest", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := shortDigest(tt.image); got != tt.want {
			t.Errorf("shortDigest(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
}

func TestBuildStatusesConvergedDivergedAndError(t *testing.T) {
	desired := spec.DesiredState{Services: map[string]spec.ServiceSpec{
		"web_a": {Name: "web_a", Image: "nginx@sha256:aaa000000000"},
		"web_b": {Name: "web_b", Image: "nginx@sha256:bbb000000000"},
		"web_c": {Name: "web_c", Image: "nginx@sha256:ccc000000000"},
	}}
	observed := spec.ObservedState{Services: map[string]spec.ServiceSpec{
		"web_a": {Name: "web_a", Image: "nginx@sha256:aaa000000000"}, // matches desired
		"web_b": {Name: "web_b", Image: "nginx@sha256:old000000000"}, // stale image: diverged
		// web_c missing entirely: diverged (pending create)
	}}
	errored := map[string]bool{"web_b": true}

	statuses := buildStatuses(desired, observed, errored, "abc123", time.Unix(1000, 0))

	got := map[string]serviceStatus{}
	for _, s := range statuses {
		got[s.Name] = s
	}
	if len(got) != 3 {
		t.Fatalf("statuses = %+v, want 3 entries", got)
	}
	if got["web_a"].State != "converged" {
		t.Errorf("web_a state = %q, want converged", got["web_a"].State)
	}
	if got["web_b"].State != "error" {
		t.Errorf("web_b state = %q, want error (diverged + errored overrides)", got["web_b"].State)
	}
	if got["web_c"].State != "diverged" {
		t.Errorf("web_c state = %q, want diverged", got["web_c"].State)
	}
	if got["web_a"].Digest != "aaa000000000" {
		t.Errorf("web_a digest = %q", got["web_a"].Digest)
	}
	if got["web_c"].Digest != "" {
		t.Errorf("web_c digest = %q, want empty (never observed)", got["web_c"].Digest)
	}
	for _, s := range got {
		if s.Commit != "abc123" {
			t.Errorf("%s commit = %q, want abc123", s.Name, s.Commit)
		}
	}
}

func TestScanTelemetryMissingFileIsNotError(t *testing.T) {
	lastEvent, errored, err := scanTelemetry(filepath.Join(t.TempDir(), "nonexistent.jsonl"))
	if err != nil {
		t.Fatalf("scanTelemetry: %v", err)
	}
	if !lastEvent.IsZero() {
		t.Errorf("lastEvent = %v, want zero", lastEvent)
	}
	if len(errored) != 0 {
		t.Errorf("errored = %+v, want empty", errored)
	}
}

func TestScanTelemetryLatestOutcomeWinsPerService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	lines := `{"run_id":"r1","stage":"verify","service":"web_a","t":"2026-01-01T00:00:00Z","fields":{"outcome":"reject"}}
{"run_id":"r1","stage":"apply","service":"web_b","t":"2026-01-01T00:00:01Z","fields":{"outcome":"error"}}
{"run_id":"r2","stage":"verify","service":"web_a","t":"2026-01-01T00:00:02Z","fields":{"outcome":"pass"}}
{"run_id":"r2","stage":"converged","t":"2026-01-01T00:00:03Z","fields":{"commit":"abc"}}
`
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	lastEvent, errored, err := scanTelemetry(path)
	if err != nil {
		t.Fatalf("scanTelemetry: %v", err)
	}
	if errored["web_a"] {
		t.Error("web_a should not be errored: its later verify event passed")
	}
	if !errored["web_b"] {
		t.Error("web_b should be errored: its only apply event failed")
	}
	want := time.Date(2026, 1, 1, 0, 0, 3, 0, time.UTC)
	if !lastEvent.Equal(want) {
		t.Errorf("lastEvent = %v, want %v", lastEvent, want)
	}
}
