package harness

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/telemetry"
)

func TestT2ConditionReplicas(t *testing.T) {
	cfg := T2Config{Drift: "replicas", Service: "web_nginx", Label: "events=on"}
	if got, want := t2Condition(cfg), "drift=replicas;service=web_nginx;events=on"; got != want {
		t.Fatalf("t2Condition = %q, want %q", got, want)
	}
}

func TestT2ConditionUnmanaged(t *testing.T) {
	cfg := T2Config{Drift: "unmanaged", Service: "decoy", Label: "events=off"}
	if got, want := t2Condition(cfg), "drift=unmanaged;service=decoy;events=off"; got != want {
		t.Fatalf("t2Condition = %q, want %q", got, want)
	}
}

func TestT2RollbackImageIsFirstPinnedTag(t *testing.T) {
	if got, want := t2RollbackImage(""), "nginx:1.24-alpine"; got != want {
		t.Fatalf("t2RollbackImage = %q, want %q", got, want)
	}
}

func TestT2DecoyName(t *testing.T) {
	cases := []struct {
		service string
		run     int
		want    string
	}{
		{"web_nginx", 1, "web_nginx-1"},
		{"api_svc", 42, "api_svc-42"},
	}
	for _, c := range cases {
		if got := t2DecoyName(c.service, c.run); got != c.want {
			t.Fatalf("t2DecoyName(%q, %d) = %q, want %q", c.service, c.run, got, c.want)
		}
	}
}

func TestT2SetEnvAppendsWhenAbsent(t *testing.T) {
	got := t2SetEnv([]string{"FOO=bar"}, "DRIFT", "3")
	want := []string{"FOO=bar", "DRIFT=3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("t2SetEnv = %v, want %v", got, want)
	}
}

func TestT2SetEnvReplacesWhenPresent(t *testing.T) {
	got := t2SetEnv([]string{"FOO=bar", "DRIFT=1"}, "DRIFT", "2")
	want := []string{"FOO=bar", "DRIFT=2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("t2SetEnv = %v, want %v", got, want)
	}
}

func TestT2SetEnvOnNilEnv(t *testing.T) {
	got := t2SetEnv(nil, "DRIFT", "1")
	want := []string{"DRIFT=1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("t2SetEnv = %v, want %v", got, want)
	}
}

func TestT2DriftMatch(t *testing.T) {
	match := t2DriftMatch("replicas", "web_nginx")

	hit := telemetry.Event{
		Stage:  telemetry.StageDrift,
		Fields: map[string]any{"kind": "replicas", "service": "web_nginx"},
	}
	if !match(hit) {
		t.Fatalf("expected match on exact kind+service")
	}

	wrongKind := telemetry.Event{
		Stage:  telemetry.StageDrift,
		Fields: map[string]any{"kind": "image", "service": "web_nginx"},
	}
	if match(wrongKind) {
		t.Fatalf("expected no match on differing kind")
	}

	wrongService := telemetry.Event{
		Stage:  telemetry.StageDrift,
		Fields: map[string]any{"kind": "replicas", "service": "other_svc"},
	}
	if match(wrongService) {
		t.Fatalf("expected no match on differing service")
	}

	wrongStage := telemetry.Event{
		Stage:  telemetry.StageConverged,
		Fields: map[string]any{"kind": "replicas", "service": "web_nginx"},
	}
	if match(wrongStage) {
		t.Fatalf("expected no match on differing stage")
	}
}

func TestT2AwaitRepairMatchesConvergedForApplyingCycle(t *testing.T) {
	events := make(chan telemetry.Event, 4)
	events <- telemetry.Event{Stage: telemetry.StageConverged, RunID: "unrelated-earlier-cycle"}
	events <- telemetry.Event{Stage: telemetry.StageApply, Service: "other_svc", RunID: "run-a"}
	events <- telemetry.Event{Stage: telemetry.StageApply, Service: "web_nginx", RunID: "run-b"}
	events <- telemetry.Event{Stage: telemetry.StageConverged, RunID: "run-b"}

	got, err := t2AwaitRepair(context.Background(), events, "web_nginx")
	if err != nil {
		t.Fatalf("t2AwaitRepair: %v", err)
	}
	if got.RunID != "run-b" {
		t.Fatalf("matched converged run_id = %q, want run-b", got.RunID)
	}
}

// TestT2AwaitRepairIgnoresConvergedFromCycleThatAppliedNothing is a
// regression test: a converged event from a cycle that never touched the
// target service must not satisfy the repair wait.
func TestT2AwaitRepairIgnoresConvergedFromCycleThatAppliedNothing(t *testing.T) {
	events := make(chan telemetry.Event, 3)
	events <- telemetry.Event{Stage: telemetry.StageConverged, RunID: "no-op-cycle"}
	events <- telemetry.Event{Stage: telemetry.StageApply, Service: "web_nginx", RunID: "run-b"}
	events <- telemetry.Event{Stage: telemetry.StageConverged, RunID: "run-b"}

	got, err := t2AwaitRepair(context.Background(), events, "web_nginx")
	if err != nil {
		t.Fatalf("t2AwaitRepair: %v", err)
	}
	if got.RunID != "run-b" {
		t.Fatalf("matched converged run_id = %q, want run-b (not the no-op cycle)", got.RunID)
	}
}

// TestT2AwaitRepairTracksMostRecentApply is a regression test for retries:
// only the most recent apply's run_id may satisfy the repair wait.
func TestT2AwaitRepairTracksMostRecentApply(t *testing.T) {
	events := make(chan telemetry.Event, 4)
	events <- telemetry.Event{Stage: telemetry.StageApply, Service: "web_nginx", RunID: "run-a"}
	events <- telemetry.Event{Stage: telemetry.StageApply, Service: "web_nginx", RunID: "run-b"}
	events <- telemetry.Event{Stage: telemetry.StageConverged, RunID: "run-a"}
	events <- telemetry.Event{Stage: telemetry.StageConverged, RunID: "run-b"}

	got, err := t2AwaitRepair(context.Background(), events, "web_nginx")
	if err != nil {
		t.Fatalf("t2AwaitRepair: %v", err)
	}
	if got.RunID != "run-b" {
		t.Fatalf("matched converged run_id = %q, want run-b (the most recent apply)", got.RunID)
	}
}

func TestT2AwaitRepairTimesOutOnContext(t *testing.T) {
	events := make(chan telemetry.Event)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := t2AwaitRepair(ctx, events, "web_nginx")
	if err == nil {
		t.Fatal("expected error on context deadline")
	}
}

func TestT2RowOkOnMatch(t *testing.T) {
	cfg := T2Config{Drift: "env", Service: "web_nginx"}
	start := time.Now()
	end := start.Add(2 * time.Second)
	row := t2Row(cfg, 1, "detect", start, end, nil)
	if row.Outcome != "ok" || row.Detail != "detect" {
		t.Fatalf("row = %+v, want outcome=ok detail=detect", row)
	}
}

func TestT2RowTimeoutOnDeadlineExceeded(t *testing.T) {
	cfg := T2Config{Drift: "env", Service: "web_nginx"}
	start := time.Now()
	row := t2Row(cfg, 1, "repair", start, start, context.DeadlineExceeded)
	if row.Outcome != "timeout" || row.Detail != "repair" {
		t.Fatalf("row = %+v, want outcome=timeout detail=repair", row)
	}
}

func TestT2RowErrorOnOtherFailure(t *testing.T) {
	cfg := T2Config{Drift: "env", Service: "web_nginx"}
	start := time.Now()
	row := t2Row(cfg, 1, "detect", start, start, context.Canceled)
	if row.Outcome != "error" {
		t.Fatalf("row.Outcome = %q, want error", row.Outcome)
	}
}
