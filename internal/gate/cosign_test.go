package gate

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/sigstore/sigstore-go/pkg/root"

	"github.com/alexmchughdev/swarmgate/internal/apply"
	"github.com/alexmchughdev/swarmgate/internal/spec"
)

// writeTestKey generates an ECDSA P-256 key pair and writes only the public
// half as a PEM file, mirroring what a policy's key_file points at.
func writeTestKey(t *testing.T, dir, name string) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := writePEM(path, &pem.Block{Type: "PUBLIC KEY", Bytes: der}); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func writePEM(path string, block *pem.Block) error {
	var buf bytes.Buffer
	if err := pem.Encode(&buf, block); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

func TestNewCosignGateLoadsPolicyKeysIntoVerifiers(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestKey(t, dir, "cosign.pub")

	g, err := NewCosignGate(Policy{Keys: []Key{{KeyFile: keyPath}}}, "")
	if err != nil {
		t.Fatalf("NewCosignGate: %v", err)
	}
	if len(g.verifiers) != 1 {
		t.Fatalf("verifiers = %d, want 1", len(g.verifiers))
	}
}

func TestNewCosignGateRejectsMissingKeyFile(t *testing.T) {
	_, err := NewCosignGate(Policy{Keys: []Key{{KeyFile: "/nonexistent/cosign.pub"}}}, "")
	if err == nil {
		t.Fatal("expected error for missing key file, got nil")
	}
}

func TestNewCosignGateRejectsMalformedKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.pub")
	if err := writePEM(path, &pem.Block{Type: "PUBLIC KEY", Bytes: []byte("not a key")}); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	_, err := NewCosignGate(Policy{Keys: []Key{{KeyFile: path}}}, "")
	if err == nil {
		t.Fatal("expected error for malformed key file, got nil")
	}
}

func TestNewCosignGateWithNoIdentitiesNeverTouchesNetwork(t *testing.T) {
	// Construction with only static keys (or none at all) must not call
	// cosign.TrustedRoot(), which fetches the Sigstore trusted root over
	// the network. Identities-only trust material loads lazily on first
	// Verify() call instead (see trustedRoot).
	g, err := NewCosignGate(Policy{}, "")
	if err != nil {
		t.Fatalf("NewCosignGate: %v", err)
	}
	if g.trusted != nil || g.trustedOK {
		t.Fatal("trusted root material was populated without any Verify() call")
	}
}

func TestVerifyRejectsUnparseableImageReference(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestKey(t, dir, "cosign.pub")
	g, err := NewCosignGate(Policy{Keys: []Key{{KeyFile: keyPath}}}, "")
	if err != nil {
		t.Fatalf("NewCosignGate: %v", err)
	}

	changes := []apply.Change{{Spec: spec.ServiceSpec{Name: "web_a", Image: "not a valid image ::::"}}}
	verdicts, err := g.Verify(context.Background(), changes)
	if err != nil {
		t.Fatalf("Verify returned a Go error instead of a rejected verdict: %v", err)
	}
	if len(verdicts) != 1 {
		t.Fatalf("verdicts = %d, want 1", len(verdicts))
	}
	if verdicts[0].Pass {
		t.Fatal("expected reject for unparseable image reference")
	}
	if !strings.Contains(verdicts[0].Reason, "parse image reference") {
		t.Fatalf("Reason = %q, want it to mention the parse failure", verdicts[0].Reason)
	}
}

// TestTrustedRootTimesOutInsteadOfHangingForever is the production-
// hardening regression test: cosign.TrustedRoot() accepts no context and
// its own HTTP client has no timeout, so a slow/interfered-with Sigstore
// network path would otherwise hang this call — and every keyless
// verification waiting on it — forever. A fetch function that never
// returns must still make trustedRoot() return within the configured
// timeout.
func TestTrustedRootTimesOutInsteadOfHangingForever(t *testing.T) {
	g := &CosignGate{
		fetchTrustedRootFn: func() (root.TrustedMaterial, error) {
			select {} // never returns, simulating a hung network call
		},
		trustedRootTimeout: 20 * time.Millisecond,
	}

	start := time.Now()
	_, err := g.trustedRoot()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("trustedRoot() = nil error, want a timeout error")
	}
	if elapsed > time.Second {
		t.Fatalf("trustedRoot() took %v to return, want it bounded by trustedRootTimeout (~20ms)", elapsed)
	}
}

// TestTrustedRootRetriesAfterTimeout is a regression test for a subtler
// bug: caching a timeout the same way a successful fetch is cached (e.g.
// via sync.Once) would permanently disable keyless verification for the
// rest of the gate's lifetime after one transient network blip. Only a
// genuinely successful fetch should be durable.
func TestTrustedRootRetriesAfterTimeout(t *testing.T) {
	// atomic: the first call's fetchTrustedRootFn goroutine is abandoned,
	// not cancelled, once its 10ms timeout fires (fetchTrustedRootWithTimeout's
	// documented tradeoff), so it is still running — and still able to
	// touch calls — when the second trustedRoot() call spawns its own
	// goroutine moments later. A plain int here raced under -race.
	var calls atomic.Int32
	g := &CosignGate{
		fetchTrustedRootFn: func() (root.TrustedMaterial, error) {
			if calls.Add(1) == 1 {
				time.Sleep(50 * time.Millisecond) // exceeds the timeout below
				return nil, nil
			}
			return fakeTrustedMaterial{}, nil
		},
		trustedRootTimeout: 10 * time.Millisecond,
	}

	if _, err := g.trustedRoot(); err == nil {
		t.Fatal("first trustedRoot() call = nil error, want a timeout error")
	}
	trusted, err := g.trustedRoot()
	if err != nil {
		t.Fatalf("second trustedRoot() call after a timeout: %v, want it to retry and succeed", err)
	}
	if trusted == nil {
		t.Fatal("second trustedRoot() call returned nil trust material")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("fetchTrustedRootFn called %d times, want 2 (no caching of the timed-out first attempt)", got)
	}
}

// TestHasValidAttestationSurfacesTrustedRootError is a regression test: a
// trusted-root fetch failure on the identity path must be distinguishable
// from a genuine "no attestation found" — collapsing both to the same
// bool previously meant a transient Sigstore outage rejected a real,
// signed image with the misleading reason "attestation required".
func TestHasValidAttestationSurfacesTrustedRootError(t *testing.T) {
	g := &CosignGate{
		policy: Policy{Identities: []Identity{{Issuer: "https://issuer.example", SubjectRegex: ".*"}}},
		fetchTrustedRootFn: func() (root.TrustedMaterial, error) {
			return nil, errors.New("boom: sigstore unreachable")
		},
		trustedRootTimeout: time.Second,
	}
	ref, err := name.ParseReference("registry.local/app:v1")
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}

	ok, err := g.hasValidAttestation(context.Background(), ref)
	if ok {
		t.Fatal("hasValidAttestation() = true, want false")
	}
	if err == nil {
		t.Fatal("hasValidAttestation() = nil error, want the trusted-root fetch error")
	}
	if !strings.Contains(err.Error(), "boom: sigstore unreachable") {
		t.Errorf("error = %v, want it to mention the underlying fetch failure", err)
	}
}

// fakeTrustedMaterial is a minimal root.TrustedMaterial for tests that
// only need trustedRoot() to report success, never a real verification.
type fakeTrustedMaterial struct{ root.TrustedMaterial }

// slsaPayload builds the in-toto statement JSON shape
// policy.AttestationToPayloadJSON actually returns for a SLSA v1 provenance
// predicate, with the given builder id nested at
// predicate.runDetails.builder.id. An empty builderID omits the builder
// object entirely, matching a real predicate that never set it, rather
// than one that set it to the empty string.
func slsaPayload(builderID string) []byte {
	builder := `{}`
	if builderID != "" {
		builder = fmt.Sprintf(`{"id":%q}`, builderID)
	}
	return []byte(fmt.Sprintf(`{
		"_type": "https://in-toto.io/Statement/v1",
		"predicateType": "https://slsa.dev/provenance/v1",
		"subject": [],
		"predicate": {
			"buildDefinition": {"buildType": "https://example.com/build"},
			"runDetails": {"builder": %s}
		}
	}`, builder))
}

// TestMatchesBuilderID covers the three cases the builder-id check exists
// for: a correct match passes, a mismatched builder rejects, and an
// attestation that never set a builder id at all rejects too (never
// treated as an unconstrained match, even against a blank policy value).
func TestMatchesBuilderID(t *testing.T) {
	const trusted = "https://github.com/org/repo/.github/workflows/build.yml@refs/heads/main"

	tests := []struct {
		name      string
		payload   []byte
		builderID string
		want      bool
	}{
		{"matching builder id passes", slsaPayload(trusted), trusted, true},
		{"wrong builder id rejects", slsaPayload("https://attacker.example/builder"), trusted, false},
		{"missing builder field rejects", slsaPayload(""), trusted, false},
		{"malformed payload rejects", []byte("not json"), trusted, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesBuilderID(tt.payload, tt.builderID); got != tt.want {
				t.Errorf("matchesBuilderID(%s, %q) = %v, want %v", tt.payload, tt.builderID, got, tt.want)
			}
		})
	}
}
