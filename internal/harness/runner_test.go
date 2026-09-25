package harness

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunWritesExactCSV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.csv")
	w, err := OpenCSVWriter(path)
	if err != nil {
		t.Fatalf("OpenCSVWriter: %v", err)
	}

	base := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	var calls []int
	err = Run(w, 3, func(run int) Row {
		calls = append(calls, run)
		start := base.Add(time.Duration(run) * time.Second)
		end := start.Add(500 * time.Millisecond)
		return Row{
			Scenario:   "scale",
			Condition:  "scale=1;changes=1;events=on",
			Run:        run,
			TStart:     start,
			TEnd:       end,
			DurationMS: 500,
			Outcome:    "ok",
			Detail:     "",
		}
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := calls, []int{1, 2, 3}; !equalInts(got, want) {
		t.Fatalf("scenario called with runs %v, want %v (sequential, 1-indexed)", got, want)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "scenario,condition,run,t_start,t_end,duration_ms,outcome,detail\n" +
		"scale,scale=1;changes=1;events=on,1,2026-07-12T10:00:01Z,2026-07-12T10:00:01.5Z,500,ok,\n" +
		"scale,scale=1;changes=1;events=on,2,2026-07-12T10:00:02Z,2026-07-12T10:00:02.5Z,500,ok,\n" +
		"scale,scale=1;changes=1;events=on,3,2026-07-12T10:00:03Z,2026-07-12T10:00:03.5Z,500,ok,\n"
	if string(got) != want {
		t.Fatalf("csv content =\n%s\nwant\n%s", got, want)
	}
}

func TestOpenCSVWriterHeaderOnlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.csv")

	w1, err := OpenCSVWriter(path)
	if err != nil {
		t.Fatalf("OpenCSVWriter (1st): %v", err)
	}
	if err := w1.Write(Row{Scenario: "scale", Run: 1, Outcome: "ok"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	w2, err := OpenCSVWriter(path)
	if err != nil {
		t.Fatalf("OpenCSVWriter (2nd, append): %v", err)
	}
	if err := w2.Write(Row{Scenario: "scale", Run: 2, Outcome: "ok"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := 0
	for _, b := range got {
		if b == '\n' {
			lines++
		}
	}
	if lines != 3 { // 1 header + 2 rows
		t.Fatalf("csv content =\n%s\nwant exactly 1 header + 2 rows (3 lines)", got)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
