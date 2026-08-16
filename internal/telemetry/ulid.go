package telemetry

import (
	"crypto/rand"

	"github.com/oklog/ulid/v2"
)

// NewRunID returns a fresh ULID identifying one reconciliation run.
// crypto/rand.Reader is safe for concurrent use, so no locking is needed.
func NewRunID() string {
	return ulid.MustNew(ulid.Now(), rand.Reader).String()
}
