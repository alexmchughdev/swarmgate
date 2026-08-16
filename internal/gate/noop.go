package gate

import (
	"context"

	"github.com/alexmchughdev/swarmgate/internal/apply"
)

// NoopGate passes every change unconditionally. Used when gate.enabled is
// false; the loop still calls through the same Gate interface, so enabling
// the gate later is a config change, not a code change.
type NoopGate struct{}

var _ Gate = NoopGate{}

func (NoopGate) Verify(_ context.Context, changes []apply.Change) ([]Verdict, error) {
	verdicts := make([]Verdict, len(changes))
	for i, c := range changes {
		verdicts[i] = Verdict{Service: c.Spec.Name, Image: c.Spec.Image, Pass: true}
	}
	return verdicts, nil
}
