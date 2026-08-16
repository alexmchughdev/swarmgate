package loop

import (
	"math/rand"
	"time"
)

// backoffBase and backoffCap bound the delay run() waits before retrying
// a cycle that failed before reaching apply: 1s on the first failure,
// doubling each consecutive failure, never exceeding 60s.
const (
	backoffBase = time.Second
	backoffCap  = 60 * time.Second
	// backoffMaxAttempt is the attempt count at which backoffBase<<attempt
	// first reaches backoffCap (2^6 = 64s > 60s); capping attempt here
	// keeps the shift from ever running toward integer overflow across a
	// long-running instance stuck in repeated failure.
	backoffMaxAttempt = 6
)

// backoff computes the delay before run() retries after a pre-apply cycle
// failure: exponential from backoffBase, capped at backoffCap, with up to
// ±20% jitter so many swarmgate instances hitting the same struggling
// dependency don't retry in lockstep. Resets to the base delay on success.
type backoff struct {
	attempt int
	rand    func() float64 // seam for deterministic tests; defaults to math/rand
}

func newBackoff() *backoff {
	return &backoff{rand: rand.Float64}
}

// next returns the delay for the current attempt and advances to the
// next one.
func (b *backoff) next() time.Duration {
	if b.attempt > backoffMaxAttempt {
		b.attempt = backoffMaxAttempt
	}
	d := backoffBase << b.attempt
	if d > backoffCap {
		d = backoffCap
	}
	b.attempt++

	jitter := 1 + (b.rand()*0.4 - 0.2) // uniform in [0.8, 1.2)
	scaled := time.Duration(float64(d) * jitter)
	if scaled > backoffCap {
		scaled = backoffCap
	}
	if scaled < 0 {
		scaled = backoffBase
	}
	return scaled
}

// reset returns the next call to next() to the base delay, for use after
// a cycle succeeds.
func (b *backoff) reset() {
	b.attempt = 0
}
