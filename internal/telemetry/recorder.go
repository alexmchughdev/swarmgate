package telemetry

import (
	"encoding/json"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Recorder receives telemetry events.
type Recorder interface {
	Emit(Event)
}

var (
	_ Recorder = (*JSONLRecorder)(nil)
	_ Recorder = NopRecorder{}
)

// JSONLRecorder appends events to an io.Writer as JSON Lines, one object
// per line. It is safe for concurrent use.
type JSONLRecorder struct {
	mu      sync.Mutex
	w       io.Writer
	now     func() time.Time
	dropped atomic.Uint64
}

// NewJSONLRecorder returns a recorder writing to w. now supplies event
// timestamps; if nil, time.Now is used.
func NewJSONLRecorder(w io.Writer, now func() time.Time) *JSONLRecorder {
	if now == nil {
		now = time.Now
	}
	return &JSONLRecorder{w: w, now: now}
}

// Emit writes e as exactly one JSON line, stamping e.T from the clock when
// unset. Emit never blocks or fails reconciliation; instead, every event
// lost to an encode or write error is counted so callers can detect and
// report an incomplete event stream (see Dropped).
func (r *JSONLRecorder) Emit(e Event) {
	if e.T.IsZero() {
		e.T = r.now()
	}
	line, err := json.Marshal(e)
	if err != nil {
		r.dropped.Add(1)
		return
	}
	line = append(line, '\n')
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, err := r.w.Write(line); err != nil || n < len(line) {
		r.dropped.Add(1)
	}
}

// Dropped reports the number of events lost to encode or write failures
// since the recorder was created. A non-zero value means the emitted JSONL
// stream is incomplete and any analysis derived from it is invalid.
func (r *JSONLRecorder) Dropped() uint64 {
	return r.dropped.Load()
}

// NopRecorder discards all events.
type NopRecorder struct{}

// Emit is a no-op.
func (NopRecorder) Emit(Event) {}
