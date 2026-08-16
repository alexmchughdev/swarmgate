package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/telemetry"
)

// tailPollInterval is how often Tail checks a JSONL file for new bytes.
// A var, not a const, so tests can shrink it.
var tailPollInterval = 50 * time.Millisecond

// Tail follows path from its current end of file, yielding one
// telemetry.Event per complete line appended after Tail starts. It does
// not replay existing content, so callers that need to observe an event
// racing their own setup must call Tail before triggering the action that
// produces it. The returned channel closes when ctx is cancelled or the
// file becomes unreadable; malformed lines are skipped rather than ending
// the stream, since a line can be observed mid-write. Log rotation is not
// handled: a truncated or replaced file stalls the tail.
func Tail(ctx context.Context, path string) (<-chan telemetry.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return nil, fmt.Errorf("seek %s: %w", path, err)
	}

	out := make(chan telemetry.Event)
	go func() {
		defer close(out)
		defer f.Close()

		var carry []byte
		buf := make([]byte, 64*1024)
		ticker := time.NewTicker(tailPollInterval)
		defer ticker.Stop()

		for {
			for {
				n, readErr := f.Read(buf)
				if n > 0 {
					carry = append(carry, buf[:n]...)
					for {
						i := bytes.IndexByte(carry, '\n')
						if i < 0 {
							break
						}
						line := bytes.TrimSpace(carry[:i])
						carry = carry[i+1:]
						if len(line) == 0 {
							continue
						}
						var e telemetry.Event
						if json.Unmarshal(line, &e) != nil {
							continue
						}
						select {
						case out <- e:
						case <-ctx.Done():
							return
						}
					}
				}
				if readErr != nil {
					break
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return out, nil
}

// AwaitEvent tails path and returns the first event for which match
// returns true, or an error once timeout elapses (or ctx is cancelled)
// with no match. Per Tail's contract, only events appended after
// AwaitEvent is called can match.
func AwaitEvent(ctx context.Context, path string, match func(telemetry.Event) bool, timeout time.Duration) (telemetry.Event, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	events, err := Tail(ctx, path)
	if err != nil {
		return telemetry.Event{}, err
	}
	for {
		select {
		case e, ok := <-events:
			if !ok {
				return telemetry.Event{}, fmt.Errorf("await event: %w", ctx.Err())
			}
			if match(e) {
				return e, nil
			}
		case <-ctx.Done():
			return telemetry.Event{}, fmt.Errorf("await event: %w", ctx.Err())
		}
	}
}
