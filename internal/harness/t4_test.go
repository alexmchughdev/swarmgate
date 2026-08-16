package harness

import (
	"testing"

	"github.com/alexmchughdev/swarmgate/internal/telemetry"
)

func TestT4Condition(t *testing.T) {
	cfg := T4Config{Offset: "apply", Service: "web_nginx", Label: "events=on"}
	got := t4Condition(cfg)
	want := "offset=apply;service=web_nginx;events=on"
	if got != want {
		t.Fatalf("t4Condition = %q, want %q", got, want)
	}
}

func TestT4ShouldFireOnEvent(t *testing.T) {
	const service = "web_nginx"

	diffEvent := telemetry.Event{Stage: telemetry.StageDiff}
	applyEventTarget := telemetry.Event{Stage: telemetry.StageApply, Service: service}
	applyEventOther := telemetry.Event{Stage: telemetry.StageApply, Service: "other_svc"}
	convergedEvent := telemetry.Event{Stage: telemetry.StageConverged}
	driftEvent := telemetry.Event{Stage: telemetry.StageDrift, Service: service}

	tests := []struct {
		offset string
		event  telemetry.Event
		want   bool
	}{
		// diff and window collapse to the same trigger: both fire on the
		// diff event and nothing else.
		{"diff", diffEvent, true},
		{"diff", applyEventTarget, false},
		{"diff", applyEventOther, false},
		{"diff", convergedEvent, false},
		{"diff", driftEvent, false},

		{"window", diffEvent, true},
		{"window", applyEventTarget, false},
		{"window", applyEventOther, false},
		{"window", convergedEvent, false},

		{"apply", diffEvent, false},
		{"apply", applyEventTarget, true},
		{"apply", applyEventOther, false},
		{"apply", convergedEvent, false},

		{"after", diffEvent, false},
		{"after", applyEventTarget, false},
		{"after", applyEventOther, false},
		{"after", convergedEvent, true},

		{"bogus", convergedEvent, false},
	}

	for _, tt := range tests {
		got := t4ShouldFireOnEvent(tt.offset, tt.event, service)
		if got != tt.want {
			t.Errorf("t4ShouldFireOnEvent(%q, stage=%s service=%s) = %v, want %v",
				tt.offset, tt.event.Stage, tt.event.Service, got, tt.want)
		}
	}
}

func TestT4DetailComposition(t *testing.T) {
	tests := []struct {
		name      string
		winner    string
		detected  bool
		revertMS  int64
		hasRevert bool
		want      string
	}{
		{
			name:   "git wins, detected, revert latency known",
			winner: "git", detected: true, revertMS: 250, hasRevert: true,
			want: "winner=git;detected=true;revert_ms=250",
		},
		{
			// The "overwritten-without-detection" composite the plan calls
			// out: git's value won but swarmgate never logged a drift event
			// about the operator's change. Derivable from winner+detected;
			// no separate field.
			name:   "git wins, never detected: overwritten-without-detection",
			winner: "git", detected: false, hasRevert: false,
			want: "winner=git;detected=false",
		},
		{
			name:   "operator wins, never detected",
			winner: "operator", detected: false, hasRevert: false,
			want: "winner=operator;detected=false",
		},
		{
			name:   "operator wins despite detection",
			winner: "operator", detected: true, hasRevert: false,
			want: "winner=operator;detected=true",
		},
		{
			name:   "git wins, detected, no revert segment when not applicable",
			winner: "git", detected: true, hasRevert: false,
			want: "winner=git;detected=true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := t4Detail(tt.winner, tt.detected, tt.revertMS, tt.hasRevert)
			if got != tt.want {
				t.Errorf("t4Detail(%q, %v, %d, %v) = %q, want %q",
					tt.winner, tt.detected, tt.revertMS, tt.hasRevert, got, tt.want)
			}
		})
	}
}
