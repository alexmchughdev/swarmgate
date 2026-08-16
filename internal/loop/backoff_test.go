package loop

import "testing"

// fixedRand returns a backoff whose jitter is always exactly the midpoint
// (factor 1.0), so the expected sequence is exact powers of two rather
// than a range.
func fixedRandBackoff() *backoff {
	return &backoff{rand: func() float64 { return 0.5 }}
}

func TestBackoffDoublesUpToCap(t *testing.T) {
	b := fixedRandBackoff()
	want := []int64{1, 2, 4, 8, 16, 32, 60, 60, 60} // seconds; caps at 60
	for i, w := range want {
		got := b.next()
		if got.Seconds() != float64(w) {
			t.Fatalf("next() call %d = %v, want %ds", i+1, got, w)
		}
	}
}

func TestBackoffResetsToBase(t *testing.T) {
	b := fixedRandBackoff()
	b.next()
	b.next()
	b.next() // attempt is now at 4s
	b.reset()
	got := b.next()
	if got.Seconds() != 1 {
		t.Fatalf("next() after reset = %v, want 1s", got)
	}
}

func TestBackoffJitterStaysWithinBounds(t *testing.T) {
	for _, r := range []float64{0, 0.25, 0.5, 0.75, 1} {
		b := &backoff{rand: func() float64 { return r }}
		d := b.next()                             // attempt 0: base 1s, jitter in [0.8, 1.2)
		if d < 800_000_000 || d > 1_200_000_000 { // nanoseconds
			t.Errorf("rand()=%v produced %v, want within [0.8s, 1.2s]", r, d)
		}
	}
}
