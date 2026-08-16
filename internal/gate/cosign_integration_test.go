//go:build integration

package gate

import (
	"context"
	"os"
	"testing"

	"github.com/alexmchughdev/swarmgate/internal/apply"
	"github.com/alexmchughdev/swarmgate/internal/spec"
)

// registryOrSkip reads SWARMGATE_TEST_REGISTRY (set by `make test-integration
// REGISTRY=...`) and skips if it isn't set, so a plain `go test -tags
// integration ./...` without a live registry fails loud with a clear reason
// rather than a confusing network error.
func registryOrSkip(t *testing.T) string {
	t.Helper()
	r := os.Getenv("SWARMGATE_TEST_REGISTRY")
	if r == "" {
		t.Skip("SWARMGATE_TEST_REGISTRY not set; run via `make test-integration REGISTRY=host:port`")
	}
	return r
}

// pipelineBuilderID is the builder id pipeline/predicate.json's
// runDetails.builder.id names — what ok-attested's real SLSA attestation
// actually claims, kept in sync with that fixture by hand since nothing
// generates one from the other.
const pipelineBuilderID = "https://github.com/alexmchughdev/swarmgate/pipeline/build.sh"

// gateForPipeline builds a CosignGate trusting pipeline/keys/cosign.pub —
// the key pipeline/build.sh signs ok-signed/ok-attested/no-attestation
// with — against the fixture images pipeline/build.sh pushes. builderID is
// only consulted when requireAttestation is true; pass "" freely for
// signature-only cases.
func gateForPipeline(t *testing.T, requireAttestation bool, builderID string) *CosignGate {
	t.Helper()
	g, err := NewCosignGate(Policy{
		Keys:               []Key{{KeyFile: "../../pipeline/keys/cosign.pub"}},
		RequireAttestation: requireAttestation,
		BuilderID:          builderID,
	}, "")
	if err != nil {
		t.Fatalf("NewCosignGate (run pipeline/build.sh first if this is a missing key file): %v", err)
	}
	return g
}

func verifyPipelineVariant(t *testing.T, g *CosignGate, registry, variant string) Verdict {
	t.Helper()
	changes := []apply.Change{{Spec: spec.ServiceSpec{
		Name:  "gate-it_" + variant,
		Image: registry + "/swarmgate-test/" + variant + ":v1",
	}}}
	verdicts, err := g.Verify(context.Background(), changes)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return verdicts[0]
}

func TestIntegrationOkSignedPasses(t *testing.T) {
	registry := registryOrSkip(t)
	v := verifyPipelineVariant(t, gateForPipeline(t, false, ""), registry, "ok-signed")
	if !v.Pass {
		t.Fatalf("ok-signed rejected: %s", v.Reason)
	}
}

func TestIntegrationUnsignedRejects(t *testing.T) {
	registry := registryOrSkip(t)
	v := verifyPipelineVariant(t, gateForPipeline(t, false, ""), registry, "unsigned")
	if v.Pass {
		t.Fatal("unsigned image passed verification")
	}
}

func TestIntegrationWrongIdentityRejects(t *testing.T) {
	registry := registryOrSkip(t)
	v := verifyPipelineVariant(t, gateForPipeline(t, false, ""), registry, "wrong-identity")
	if v.Pass {
		t.Fatal("wrong-identity image passed verification against the trusted key")
	}
}

func TestIntegrationOkAttestedPasses(t *testing.T) {
	registry := registryOrSkip(t)
	v := verifyPipelineVariant(t, gateForPipeline(t, true, pipelineBuilderID), registry, "ok-attested")
	if !v.Pass {
		t.Fatalf("ok-attested rejected: %s", v.Reason)
	}
}

// TestIntegrationOkAttestedRejectsWrongBuilderID proves the builder-id check
// against a real cosign-signed attestation, not just the hand-crafted JSON
// TestMatchesBuilderID exercises: the exact same ok-attested image and key
// that TestIntegrationOkAttestedPasses accepts is rejected once the policy
// names a builder id the real attestation doesn't carry.
func TestIntegrationOkAttestedRejectsWrongBuilderID(t *testing.T) {
	registry := registryOrSkip(t)
	v := verifyPipelineVariant(t, gateForPipeline(t, true, "https://attacker.example/builder"), registry, "ok-attested")
	if v.Pass {
		t.Fatal("ok-attested passed against a policy naming a builder id its attestation doesn't carry")
	}
	if v.Reason != "attestation required" {
		t.Fatalf("Reason = %q, want %q", v.Reason, "attestation required")
	}
}

func TestIntegrationNoAttestationRejectsWithReason(t *testing.T) {
	registry := registryOrSkip(t)
	v := verifyPipelineVariant(t, gateForPipeline(t, true, pipelineBuilderID), registry, "no-attestation")
	if v.Pass {
		t.Fatal("no-attestation image passed a require_attestation policy")
	}
	if v.Reason != "attestation required" {
		t.Fatalf("Reason = %q, want %q", v.Reason, "attestation required")
	}
}
