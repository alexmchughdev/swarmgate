package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/telemetry"
)

func init() {
	// Keep tests fast without depending on wall-clock tuning per test.
	tailPollInterval = 5 * time.Millisecond
}

func appendLine(t *testing.T, f *os.File, e telemetry.Event) {
	t.Helper()
	line, err := jsonMarshalEvent(e)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatalf("write line: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

// jsonMarshalEvent reuses telemetry.Event's own MarshalJSON so fixtures use
// the real wire format rather than a hand-rolled approximation.
func jsonMarshalEvent(e telemetry.Event) ([]byte, error) {
	return e.MarshalJSON()
}

func TestTailOnlyYieldsLinesAppendedAfterStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	appendLine(t, f, telemetry.Event{RunID: "pre-existing", Stage: telemetry.StagePoll})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := Tail(ctx, path)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}

	appendLine(t, f, telemetry.Event{RunID: "new-1", Stage: telemetry.StagePoll})
	appendLine(t, f, telemetry.Event{RunID: "new-2", Stage: telemetry.StageConverged})

	var got []string
	for i := 0; i < 2; i++ {
		select {
		case e := <-events:
			got = append(got, e.RunID)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for event %d", i)
		}
	}
	if got[0] != "new-1" || got[1] != "new-2" {
		t.Fatalf("got %v, want [new-1 new-2] (pre-existing line must not be replayed)", got)
	}
}

func TestTailClosesOnCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	events, err := Tail(ctx, path)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	cancel()

	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("expected channel to close on cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not close within 2s of cancellation")
	}
}

func TestTailSkipsMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := Tail(ctx, path)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}

	if _, err := f.WriteString("not json\n\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	appendLine(t, f, telemetry.Event{RunID: "ok-1", Stage: telemetry.StagePoll})

	select {
	case e := <-events:
		if e.RunID != "ok-1" {
			t.Fatalf("got %q, want ok-1 (malformed/blank lines must be skipped)", e.RunID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the valid event")
	}
}

func TestAwaitEventMatchesOnAppendedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	done := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		appendLine(t, f, telemetry.Event{RunID: "r1", Stage: telemetry.StageConverged, Fields: map[string]any{"commit": "abc123"}})
		close(done)
	}()

	match := func(e telemetry.Event) bool {
		return e.Stage == telemetry.StageConverged && e.Fields["commit"] == "abc123"
	}
	e, err := AwaitEvent(context.Background(), path, match, 2*time.Second)
	if err != nil {
		t.Fatalf("AwaitEvent: %v", err)
	}
	if e.RunID != "r1" {
		t.Fatalf("matched event RunID = %q, want r1", e.RunID)
	}
	<-done
}

func TestAwaitEventTimesOutWithoutMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	appendLine(t, f, telemetry.Event{RunID: "r1", Stage: telemetry.StagePoll})

	_, err = AwaitEvent(context.Background(), path, func(telemetry.Event) bool { return false }, 50*time.Millisecond)
	if err == nil {
		t.Fatal("AwaitEvent() = nil error, want timeout")
	}
}
