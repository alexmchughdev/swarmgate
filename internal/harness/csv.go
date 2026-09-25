package harness

import (
	"encoding/csv"
	"fmt"
	"os"
	"time"
)

// csvHeader is the fixed results.csv column order.
var csvHeader = []string{"scenario", "condition", "run", "t_start", "t_end", "duration_ms", "outcome", "detail"}

// Row is one results.csv record. Scenario is one of scale..verify; Outcome is one
// of ok|timeout|error; Condition and Detail are scenario-specific free text.
type Row struct {
	Scenario   string
	Condition  string
	Run        int
	TStart     time.Time
	TEnd       time.Time
	DurationMS int64
	Outcome    string
	Detail     string
}

func (r Row) record() []string {
	return []string{
		r.Scenario,
		r.Condition,
		fmt.Sprintf("%d", r.Run),
		r.TStart.UTC().Format(time.RFC3339Nano),
		r.TEnd.UTC().Format(time.RFC3339Nano),
		fmt.Sprintf("%d", r.DurationMS),
		r.Outcome,
		r.Detail,
	}
}

// CSVWriter appends Rows to a results file, writing the header exactly once.
// It is not safe for concurrent use: harness runs are sequential (runner.Run).
type CSVWriter struct {
	f *os.File
	w *csv.Writer
}

// OpenCSVWriter opens path for appending, creating it and writing the header
// if it does not already exist or is empty. An existing non-empty file (a
// prior run's output) is appended to as-is: the header is written at
// most once per file.
func OpenCSVWriter(path string) (*CSVWriter, error) {
	info, statErr := os.Stat(path)
	needsHeader := statErr != nil || info.Size() == 0

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	w := csv.NewWriter(f)
	if needsHeader {
		if err := w.Write(csvHeader); err != nil {
			f.Close()
			return nil, fmt.Errorf("write header %s: %w", path, err)
		}
		w.Flush()
		if err := w.Error(); err != nil {
			f.Close()
			return nil, fmt.Errorf("flush header %s: %w", path, err)
		}
	}
	return &CSVWriter{f: f, w: w}, nil
}

// Write appends one row and flushes immediately, so a harness process
// killed mid-run leaves every completed row on disk.
func (c *CSVWriter) Write(r Row) error {
	if err := c.w.Write(r.record()); err != nil {
		return fmt.Errorf("write row: %w", err)
	}
	c.w.Flush()
	return c.w.Error()
}

// Close closes the underlying file.
func (c *CSVWriter) Close() error {
	return c.f.Close()
}
