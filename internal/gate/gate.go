package gate

import (
	"context"

	"github.com/alexmchughdev/swarmgate/internal/apply"
)

// Verdict is one service's gate decision. Reason is populated only when
// Pass is false.
type Verdict struct {
	Service string
	Image   string
	Pass    bool
	Reason  string
}

// Gate verifies a set of changes against supply-chain policy before they
// are applied. Implementations must fail closed: any verification error —
// registry unreachable, malformed signature, expired certificate — is a
// reject, never a silent pass.
//
// Verify evaluates every change handed to it and returns one Verdict per
// change, in the same order; it does not decide whether a reject blocks
// just that service or the whole cycle — that policy (gate.mode) belongs
// to the loop, not the gate.
type Gate interface {
	Verify(ctx context.Context, changes []apply.Change) ([]Verdict, error)
}
