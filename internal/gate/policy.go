package gate

import (
	"errors"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Identity is one Fulcio/Rekor keyless identity a signature may match.
// Both fields are required by LoadPolicy: cosign treats a blank Issuer or
// SubjectRegex as "no constraint on this half of the identity," so a
// half-filled entry is not a narrower match, it's a bypass of that half
// entirely.
type Identity struct {
	Issuer       string `yaml:"issuer"`
	SubjectRegex string `yaml:"subject_regex"`
}

// Key is one static public key a signature may match.
type Key struct {
	KeyFile string `yaml:"key_file"`
}

// Policy is the fixed gate policy schema (see swarmgate.yaml's gate.policy_file).
type Policy struct {
	Identities []Identity `yaml:"identities"`
	Keys       []Key      `yaml:"keys"`

	// RequireAttestation is the global default; PerStack overrides it for
	// specific stacks by name.
	RequireAttestation bool            `yaml:"require_attestation"`
	PerStack           map[string]bool `yaml:"per_stack"`

	// BuilderID is the exact SLSA provenance builder identity
	// (predicate.runDetails.builder.id) an attestation must carry to
	// satisfy require_attestation. Matching the predicate type alone only
	// proves *an* attestation of the right shape exists, not that it was
	// produced by a builder this policy actually trusts — a signed but
	// vacuous or misleading provenance body would otherwise pass. Required
	// whenever attestation is required, globally or via PerStack.
	BuilderID string `yaml:"builder_id"`
}

// RequireAttestationFor reports whether stack must carry a verified SLSA
// provenance attestation, applying PerStack's override over the global
// default when present.
func (p Policy) RequireAttestationFor(stack string) bool {
	if v, ok := p.PerStack[stack]; ok {
		return v
	}
	return p.RequireAttestation
}

// requiresAttestation reports whether any stack could require attestation
// under p — the global default, or any true PerStack override.
func requiresAttestation(p Policy) bool {
	if p.RequireAttestation {
		return true
	}
	for _, v := range p.PerStack {
		if v {
			return true
		}
	}
	return false
}

// LoadPolicy reads and validates a gate policy file: at least one of
// identities/keys must be present; every identity must set both issuer and
// subject_regex (an empty field is not a narrower constraint to cosign, it
// is no constraint at all — see the Identity doc comment); every
// subject_regex must compile. All validation errors are collected and
// returned joined, matching the rest of this codebase's config-loading
// convention.
func LoadPolicy(path string) (Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, fmt.Errorf("read policy %s: %w", path, err)
	}
	var p Policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return Policy{}, fmt.Errorf("parse policy %s: %w", path, err)
	}

	var errs []error
	if len(p.Identities) == 0 && len(p.Keys) == 0 {
		errs = append(errs, errors.New("policy must specify at least one of identities or keys"))
	}
	if requiresAttestation(p) && p.BuilderID == "" {
		errs = append(errs, errors.New("builder_id is required when require_attestation is true (globally or via per_stack)"))
	}
	for i, id := range p.Identities {
		if id.Issuer == "" {
			errs = append(errs, fmt.Errorf("identities[%d]: issuer is required (cosign treats a blank issuer as unconstrained, matching any)", i))
		}
		if id.SubjectRegex == "" {
			errs = append(errs, fmt.Errorf("identities[%d]: subject_regex is required (use \".*\" if any subject is genuinely intended)", i))
			continue
		}
		if _, err := regexp.Compile(id.SubjectRegex); err != nil {
			errs = append(errs, fmt.Errorf("identities[%d].subject_regex: %w", i, err))
		}
	}
	if len(errs) > 0 {
		return Policy{}, errors.Join(errs...)
	}
	return p, nil
}
