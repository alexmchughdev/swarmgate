package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const testRunID = "01JZXK5E8ZT2Q4R6S8V0W2X4Y6"

func fixedNow() time.Time {
	return time.Date(2026, 7, 5, 10, 0, 0, 123456000, time.UTC)
}

// TestJSONLRecorderGolden pins the exact wire format for every stage
// variant. encoding/json sorts map keys, so fields appear alphabetically.
func TestJSONLRecorderGolden(t *testing.T) {
	tests := []struct {
		name  string
		event Event
		want  string
	}{
		{
			name: "poll",
			event: Event{
				RunID:  testRunID,
				Stage:  StagePoll,
				DurMS:  42,
				Fields: map[string]any{"commit": "9f2c4e1"},
			},
			want: `{"run_id":"` + testRunID + `","stage":"poll","t":"2026-07-05T10:00:00.123456Z","dur_ms":42,"fields":{"commit":"9f2c4e1"}}`,
		},
		{
			name:  "parse no fields",
			event: Event{RunID: testRunID, Stage: StageParse},
			want:  `{"run_id":"` + testRunID + `","stage":"parse","t":"2026-07-05T10:00:00.123456Z"}`,
		},
		{
			name: "resolve",
			event: Event{
				RunID:   testRunID,
				Stage:   StageResolve,
				Service: "web",
				Fields: map[string]any{
					"image":  "nginx:1.27",
					"digest": "sha256:deadbeef",
				},
			},
			want: `{"run_id":"` + testRunID + `","stage":"resolve","service":"web","t":"2026-07-05T10:00:00.123456Z","fields":{"digest":"sha256:deadbeef","image":"nginx:1.27"}}`,
		},
		{
			name: "observe",
			event: Event{
				RunID: testRunID,
				Stage: StageObserve,
				DurMS: 7,
			},
			want: `{"run_id":"` + testRunID + `","stage":"observe","t":"2026-07-05T10:00:00.123456Z","dur_ms":7}`,
		},
		{
			name: "diff",
			event: Event{
				RunID: testRunID,
				Stage: StageDiff,
				Fields: map[string]any{
					"creates": 1,
					"updates": 2,
					"removes": 0,
				},
			},
			want: `{"run_id":"` + testRunID + `","stage":"diff","t":"2026-07-05T10:00:00.123456Z","fields":{"creates":1,"removes":0,"updates":2}}`,
		},
		{
			name: "verify pass",
			event: Event{
				RunID:   testRunID,
				Stage:   StageVerify,
				Service: "web",
				Fields: map[string]any{
					"image":   "nginx:1.27",
					"outcome": "pass",
				},
			},
			want: `{"run_id":"` + testRunID + `","stage":"verify","service":"web","t":"2026-07-05T10:00:00.123456Z","fields":{"image":"nginx:1.27","outcome":"pass"}}`,
		},
		{
			name: "verify reject",
			event: Event{
				RunID:   testRunID,
				Stage:   StageVerify,
				Service: "web",
				Fields: map[string]any{
					"image":   "nginx:1.27",
					"outcome": "reject",
					"reason":  "signature mismatch",
				},
			},
			want: `{"run_id":"` + testRunID + `","stage":"verify","service":"web","t":"2026-07-05T10:00:00.123456Z","fields":{"image":"nginx:1.27","outcome":"reject","reason":"signature mismatch"}}`,
		},
		{
			name: "apply ok",
			event: Event{
				RunID:   testRunID,
				Stage:   StageApply,
				Service: "web",
				DurMS:   120,
				Fields: map[string]any{
					"action":  "create",
					"outcome": "ok",
				},
			},
			want: `{"run_id":"` + testRunID + `","stage":"apply","service":"web","t":"2026-07-05T10:00:00.123456Z","dur_ms":120,"fields":{"action":"create","outcome":"ok"}}`,
		},
		{
			name: "apply error",
			event: Event{
				RunID:   testRunID,
				Stage:   StageApply,
				Service: "web",
				Fields: map[string]any{
					"action":  "update",
					"outcome": "error",
					"error":   "service update failed",
				},
			},
			want: `{"run_id":"` + testRunID + `","stage":"apply","service":"web","t":"2026-07-05T10:00:00.123456Z","fields":{"action":"update","error":"service update failed","outcome":"error"}}`,
		},
		{
			name: "converged",
			event: Event{
				RunID: testRunID,
				Stage: StageConverged,
				Fields: map[string]any{
					"commit":   "9f2c4e1",
					"services": 3,
				},
			},
			want: `{"run_id":"` + testRunID + `","stage":"converged","t":"2026-07-05T10:00:00.123456Z","fields":{"commit":"9f2c4e1","services":3}}`,
		},
		{
			name: "drift",
			event: Event{
				RunID: testRunID,
				Stage: StageDrift,
				Fields: map[string]any{
					"origin":  "event",
					"kind":    "replicas",
					"service": "web",
				},
			},
			want: `{"run_id":"` + testRunID + `","stage":"drift","t":"2026-07-05T10:00:00.123456Z","fields":{"kind":"replicas","origin":"event","service":"web"}}`,
		},
		{
			name: "error",
			event: Event{
				RunID: testRunID,
				Stage: StageError,
				Fields: map[string]any{
					"applied":  []string{"a"},
					"pending":  []string{"b", "c"},
					"believed": "partial",
					"error":    "apply aborted",
				},
			},
			want: `{"run_id":"` + testRunID + `","stage":"error","t":"2026-07-05T10:00:00.123456Z","fields":{"applied":["a"],"believed":"partial","error":"apply aborted","pending":["b","c"]}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			rec := NewJSONLRecorder(&buf, fixedNow)
			rec.Emit(tt.event)
			got := buf.String()
			if got != tt.want+"\n" {
				t.Errorf("line mismatch\n got: %q\nwant: %q", got, tt.want+"\n")
			}
		})
	}
}

func TestJSONLRecorderTimestamp(t *testing.T) {
	t.Run("zero t stamped from clock", func(t *testing.T) {
		var buf bytes.Buffer
		rec := NewJSONLRecorder(&buf, fixedNow)
		rec.Emit(Event{RunID: testRunID, Stage: StagePoll})
		if !strings.Contains(buf.String(), `"t":"2026-07-05T10:00:00.123456Z"`) {
			t.Errorf("expected clock timestamp, got %q", buf.String())
		}
	})

	t.Run("non-zero t preserved and normalized to UTC", func(t *testing.T) {
		var buf bytes.Buffer
		rec := NewJSONLRecorder(&buf, fixedNow)
		loc := time.FixedZone("UTC+2", 2*60*60)
		rec.Emit(Event{
			RunID: testRunID,
			Stage: StagePoll,
			T:     time.Date(2026, 1, 2, 5, 4, 3, 0, loc),
		})
		if !strings.Contains(buf.String(), `"t":"2026-01-02T03:04:03.000000Z"`) {
			t.Errorf("expected preserved timestamp, got %q", buf.String())
		}
	})
}

func TestJSONLRecorderConcurrent(t *testing.T) {
	const (
		goroutines = 16
		perG       = 50
	)
	var buf bytes.Buffer
	rec := NewJSONLRecorder(&buf, fixedNow)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				rec.Emit(Event{
					RunID:  testRunID,
					Stage:  StageApply,
					Fields: map[string]any{"action": "update", "outcome": "ok"},
				})
			}
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if got, want := len(lines), goroutines*perG; got != want {
		t.Fatalf("line count = %d, want %d", got, want)
	}
	for i, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("line %d is not valid JSON: %q", i, line)
		}
	}
}

// failingWriter fails every write after the first n succeed.
type failingWriter struct {
	remaining int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, errors.New("write failed")
	}
	w.remaining--
	return len(p), nil
}

func TestJSONLRecorderDropped(t *testing.T) {
	r := NewJSONLRecorder(&failingWriter{remaining: 2}, fixedNow)
	for i := 0; i < 5; i++ {
		r.Emit(Event{RunID: testRunID, Stage: StagePoll})
	}
	if got, want := r.Dropped(), uint64(3); got != want {
		t.Fatalf("Dropped() = %d, want %d", got, want)
	}
}

func TestJSONLRecorderDroppedZeroOnSuccess(t *testing.T) {
	var buf bytes.Buffer
	r := NewJSONLRecorder(&buf, fixedNow)
	r.Emit(Event{RunID: testRunID, Stage: StagePoll})
	if got := r.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}
}

func TestNopRecorder(t *testing.T) {
	// Must not panic; discards everything.
	NopRecorder{}.Emit(Event{RunID: testRunID, Stage: StagePoll})
}

func TestNewRunID(t *testing.T) {
	crockford := regexp.MustCompile(`^[0-9ABCDEFGHJKMNPQRSTVWXYZ]{26}$`)
	seen := make(map[string]struct{})
	for i := 0; i < 1000; i++ {
		id := NewRunID()
		if !crockford.MatchString(id) {
			t.Fatalf("NewRunID() = %q, not a 26-char Crockford ULID", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("NewRunID() returned duplicate %q", id)
		}
		seen[id] = struct{}{}
	}
}
