package telemetry

import (
	"encoding/json"
	"time"
)

// Stage values for Event.Stage.
const (
	StagePoll      = "poll"
	StageParse     = "parse"
	StageResolve   = "resolve"
	StageObserve   = "observe"
	StageDiff      = "diff"
	StageVerify    = "verify"
	StageApply     = "apply"
	StageConverged = "converged"
	StageDrift     = "drift"
	StageError     = "error"
)

// timeLayout renders RFC3339 UTC with exactly six fractional digits.
// time.RFC3339Nano is unsuitable because it trims trailing zeros, which
// would make the wire format vary between events.
const timeLayout = "2006-01-02T15:04:05.000000Z07:00"

// Event is a single telemetry record. Fields are declared in wire order;
// encoding/json emits struct fields in declaration order.
//
// DurMS measures the call each stage's name claims for poll, parse, and
// observe (one Fetch/Parse/Snapshot call). resolve and verify run one
// batched call per cycle covering every service, so each per-service
// event carries the whole batch's duration, not an individual service's
// share of it. converged's DurMS measures the entire cycle from before
// poll, not just the convergence wait: it is elapsed-since-cycle-start,
// stamped at the point a cycle is confirmed to have applied and converged.
type Event struct {
	RunID   string         `json:"run_id"`
	Stage   string         `json:"stage"`
	Service string         `json:"service,omitempty"`
	T       time.Time      `json:"t"`
	DurMS   int64          `json:"dur_ms,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// MarshalJSON serializes the event with the timestamp normalized to UTC and
// formatted with a fixed number of fractional digits.
func (e Event) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		RunID   string         `json:"run_id"`
		Stage   string         `json:"stage"`
		Service string         `json:"service,omitempty"`
		T       string         `json:"t"`
		DurMS   int64          `json:"dur_ms,omitempty"`
		Fields  map[string]any `json:"fields,omitempty"`
	}{
		RunID:   e.RunID,
		Stage:   e.Stage,
		Service: e.Service,
		T:       e.T.UTC().Format(timeLayout),
		DurMS:   e.DurMS,
		Fields:  e.Fields,
	})
}
