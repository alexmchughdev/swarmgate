package harness

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"

	"github.com/alexmchughdev/swarmgate/internal/telemetry"
)

func TestT6Condition(t *testing.T) {
	cfg := T6Config{Case: "wrong-identity", Mode: "abort-cycle", Label: "events=on"}
	got := t6Condition(cfg)
	want := "case=wrong-identity;mode=abort-cycle;events=on"
	if got != want {
		t.Fatalf("t6Condition = %q, want %q", got, want)
	}
}

func TestT6ServiceName(t *testing.T) {
	if got := t6ServiceName("web1", 3); got != "web1-3" {
		t.Fatalf("t6ServiceName = %q, want %q", got, "web1-3")
	}
}

func TestT6Image(t *testing.T) {
	tests := []struct {
		c    string
		want string
	}{
		{"ok", "registry:5000/swarmgate-test/ok-signed:v1"},
		{"unsigned", "registry:5000/swarmgate-test/unsigned:v1"},
		{"wrong-identity", "registry:5000/swarmgate-test/wrong-identity:v1"},
		{"no-attestation", "registry:5000/swarmgate-test/no-attestation:v1"},
		{"tag-repoint", "registry:5000/swarmgate-test/tag-repoint:v1"},
	}
	for _, tt := range tests {
		got, err := t6Image(T6Config{Case: tt.c, Registry: "registry:5000"})
		if err != nil {
			t.Fatalf("t6Image(%q): %v", tt.c, err)
		}
		if got != tt.want {
			t.Errorf("t6Image(%q) = %q, want %q", tt.c, got, tt.want)
		}
	}
}

func TestT6ImageRejectsUnknownCase(t *testing.T) {
	if _, err := t6Image(T6Config{Case: "bogus", Registry: "registry:5000"}); err == nil {
		t.Fatal("expected error for unknown case")
	}
}

type fakeT6API struct {
	svc swarm.Service
	err error
}

func (f *fakeT6API) ServiceInspectWithRaw(context.Context, string, swarm.ServiceInspectOptions) (swarm.Service, []byte, error) {
	return f.svc, nil, f.err
}

func TestT6DeployedDigest(t *testing.T) {
	api := &fakeT6API{svc: swarm.Service{Spec: swarm.ServiceSpec{
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{Image: "registry:5000/swarmgate-test/unsigned@sha256:abc123"},
		},
	}}}
	got, err := t6DeployedDigest(context.Background(), api, "web1-1")
	if err != nil {
		t.Fatalf("t6DeployedDigest: %v", err)
	}
	if got != "sha256:abc123" {
		t.Fatalf("digest = %q, want %q", got, "sha256:abc123")
	}
}

func TestT6DeployedDigestRejectsTagOnlyImage(t *testing.T) {
	api := &fakeT6API{svc: swarm.Service{Spec: swarm.ServiceSpec{
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{Image: "registry:5000/swarmgate-test/unsigned:v1"},
		},
	}}}
	if _, err := t6DeployedDigest(context.Background(), api, "web1-1"); err == nil {
		t.Fatal("expected error for a tag-only (non-digest-pinned) image")
	}
}

func TestAwaitFromChannelMatchesAndIgnoresOthers(t *testing.T) {
	events := make(chan telemetry.Event, 3)
	events <- telemetry.Event{Stage: telemetry.StageDiff}
	events <- telemetry.Event{Stage: telemetry.StageVerify, Service: "other"}
	events <- telemetry.Event{Stage: telemetry.StageVerify, Service: "web1-1"}

	match := func(e telemetry.Event) bool {
		return e.Stage == telemetry.StageVerify && e.Service == "web1-1"
	}
	got, err := awaitFromChannel(context.Background(), events, match)
	if err != nil {
		t.Fatalf("awaitFromChannel: %v", err)
	}
	if got.Service != "web1-1" {
		t.Fatalf("matched event = %+v, want service web1-1", got)
	}
}

func TestAwaitFromChannelTimesOutOnContext(t *testing.T) {
	events := make(chan telemetry.Event)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := awaitFromChannel(ctx, events, func(telemetry.Event) bool { return true })
	if err == nil {
		t.Fatal("expected error on context deadline")
	}
}

func TestAwaitFromChannelReturnsErrorOnClose(t *testing.T) {
	events := make(chan telemetry.Event)
	close(events)

	_, err := awaitFromChannel(context.Background(), events, func(telemetry.Event) bool { return true })
	if err == nil {
		t.Fatal("expected error on closed channel")
	}
}
