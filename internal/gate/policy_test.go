package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePolicy(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestLoadPolicyWithIdentities(t *testing.T) {
	path := writePolicy(t, `
identities:
  - issuer: "https://token.actions.githubusercontent.com"
    subject_regex: "^https://github.com/org/repo/.*$"
`)
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if len(p.Identities) != 1 || p.Identities[0].Issuer != "https://token.actions.githubusercontent.com" {
		t.Fatalf("Identities = %+v", p.Identities)
	}
	if p.Identities[0].SubjectRegex != "^https://github.com/org/repo/.*$" {
		t.Fatalf("SubjectRegex = %q", p.Identities[0].SubjectRegex)
	}
}

func TestLoadPolicyWithKeys(t *testing.T) {
	path := writePolicy(t, `
keys:
  - key_file: "/etc/swarmgate/cosign.pub"
`)
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if len(p.Keys) != 1 || p.Keys[0].KeyFile != "/etc/swarmgate/cosign.pub" {
		t.Fatalf("Keys = %+v", p.Keys)
	}
}

func TestLoadPolicyRequiresIdentitiesOrKeys(t *testing.T) {
	path := writePolicy(t, `require_attestation: true`)
	_, err := LoadPolicy(path)
	if err == nil {
		t.Fatal("LoadPolicy() = nil error, want a validation error")
	}
}

func TestLoadPolicyRejectsBadSubjectRegex(t *testing.T) {
	path := writePolicy(t, `
identities:
  - issuer: "https://token.actions.githubusercontent.com"
    subject_regex: "(unclosed"
`)
	_, err := LoadPolicy(path)
	if err == nil {
		t.Fatal("LoadPolicy() = nil error, want a regex compile error")
	}
}

// TestLoadPolicyRejectsBlankIssuer is a regression test: cosign treats a
// blank Issuer as "no constraint," so accepting one here would silently
// turn a policy entry into "any issuer, matching subject_regex" instead
// of the narrower match an operator would expect from a filled-in policy.
func TestLoadPolicyRejectsBlankIssuer(t *testing.T) {
	path := writePolicy(t, `
identities:
  - issuer: ""
    subject_regex: "^https://github.com/org/repo/.*$"
`)
	_, err := LoadPolicy(path)
	if err == nil {
		t.Fatal("LoadPolicy() = nil error, want a blank-issuer validation error")
	}
}

// TestLoadPolicyRejectsBlankSubjectRegex is a regression test: cosign
// treats a blank SubjectRegex as "no constraint," so accepting one here
// would silently turn a policy entry into "any subject from this issuer"
// instead of the narrower match an operator would expect.
func TestLoadPolicyRejectsBlankSubjectRegex(t *testing.T) {
	path := writePolicy(t, `
identities:
  - issuer: "https://token.actions.githubusercontent.com"
    subject_regex: ""
`)
	_, err := LoadPolicy(path)
	if err == nil {
		t.Fatal("LoadPolicy() = nil error, want a blank-subject_regex validation error")
	}
}

// TestLoadPolicyRejectsFullyBlankIdentity is a regression test for the
// worst case of the above: an identity entry with neither field set would
// otherwise match any keyless-signed image from any issuer whatsoever —
// a full bypass of the identity check.
func TestLoadPolicyRejectsFullyBlankIdentity(t *testing.T) {
	path := writePolicy(t, `
identities:
  - issuer: ""
    subject_regex: ""
`)
	_, err := LoadPolicy(path)
	if err == nil {
		t.Fatal("LoadPolicy() = nil error, want validation errors for both blank fields")
	}
}

// TestLoadPolicyRequiresBuilderIDWhenAttestationRequired is a regression
// test: matching an attestation's predicate type alone only proves one of
// the right shape exists, not who produced it. Without a required
// builder_id, a signed but vacuous or misleading provenance body would
// satisfy require_attestation just as well as a real one.
func TestLoadPolicyRequiresBuilderIDWhenAttestationRequired(t *testing.T) {
	path := writePolicy(t, `
keys:
  - key_file: "/etc/swarmgate/cosign.pub"
require_attestation: true
`)
	_, err := LoadPolicy(path)
	if err == nil {
		t.Fatal("LoadPolicy() = nil error, want a missing-builder_id validation error")
	}
	if !strings.Contains(err.Error(), "builder_id") {
		t.Fatalf("error %q does not mention builder_id", err)
	}
}

// TestLoadPolicyRequiresBuilderIDForPerStackOverride is the same regression
// as above, but for a policy whose global default is false and only turns
// attestation on for specific stacks via per_stack — builder_id must still
// be required, since some stack really will need it checked.
func TestLoadPolicyRequiresBuilderIDForPerStackOverride(t *testing.T) {
	path := writePolicy(t, `
keys:
  - key_file: "/etc/swarmgate/cosign.pub"
require_attestation: false
per_stack:
  high-risk: true
`)
	_, err := LoadPolicy(path)
	if err == nil {
		t.Fatal("LoadPolicy() = nil error, want a missing-builder_id validation error")
	}
	if !strings.Contains(err.Error(), "builder_id") {
		t.Fatalf("error %q does not mention builder_id", err)
	}
}

func TestLoadPolicyAcceptsBuilderIDWithAttestation(t *testing.T) {
	path := writePolicy(t, `
keys:
  - key_file: "/etc/swarmgate/cosign.pub"
require_attestation: true
builder_id: "https://example.com/builder"
`)
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if p.BuilderID != "https://example.com/builder" {
		t.Fatalf("BuilderID = %q", p.BuilderID)
	}
}

// TestLoadPolicyAllowsMissingBuilderIDWithoutAttestation is the converse:
// builder_id has nothing to constrain when no stack ever requires
// attestation, so it must not be forced on every policy regardless.
func TestLoadPolicyAllowsMissingBuilderIDWithoutAttestation(t *testing.T) {
	path := writePolicy(t, `
keys:
  - key_file: "/etc/swarmgate/cosign.pub"
`)
	if _, err := LoadPolicy(path); err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
}

func TestLoadPolicyCollectsMultipleErrors(t *testing.T) {
	path := writePolicy(t, `
identities:
  - issuer: "a"
    subject_regex: "(bad"
  - issuer: "b"
    subject_regex: "(alsobad"
`)
	_, err := LoadPolicy(path)
	if err == nil {
		t.Fatal("LoadPolicy() = nil error, want two collected regex errors")
	}
}

func TestLoadPolicyMissingFile(t *testing.T) {
	_, err := LoadPolicy(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil {
		t.Fatal("LoadPolicy() = nil error, want a read error")
	}
}

func TestRequireAttestationForGlobalDefault(t *testing.T) {
	p := Policy{RequireAttestation: true}
	if !p.RequireAttestationFor("any-stack") {
		t.Fatal("expected global default true")
	}
}

func TestRequireAttestationForPerStackOverride(t *testing.T) {
	p := Policy{
		RequireAttestation: true,
		PerStack:           map[string]bool{"low-risk": false},
	}
	if p.RequireAttestationFor("low-risk") {
		t.Fatal("expected per_stack override to false")
	}
	if !p.RequireAttestationFor("other-stack") {
		t.Fatal("expected global default true for a stack without an override")
	}
}
