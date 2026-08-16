package gate

import (
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sigstore/cosign/v3/pkg/cosign"
	"github.com/sigstore/cosign/v3/pkg/oci"
	ociremote "github.com/sigstore/cosign/v3/pkg/oci/remote"
	"github.com/sigstore/cosign/v3/pkg/policy"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/alexmchughdev/swarmgate/internal/apply"
	"github.com/alexmchughdev/swarmgate/internal/spec"
)

// trustedRootTimeout bounds the Sigstore TUF trusted-root fetch.
// cosign.TrustedRoot() takes no context, and its own internal HTTP client
// has no timeout of its own — a slow or interfered-with Sigstore network
// path would otherwise hang this call forever, and with it every
// keyless/identity verification (and, transitively, the whole reconcile
// cycle waiting on gate.Verify) indefinitely. Racing it against a timer in
// its own goroutine is the only way to bound a function that accepts no
// context; if it never returns, that one goroutine is leaked rather than
// the caller.
const trustedRootTimeout = 30 * time.Second

// slsaProvenancePredicateType is cosign's short name for the SLSA v1
// provenance predicate (resolved internally to
// https://slsa.dev/provenance/v1), matching the --type flag pipeline/build.sh
// passes to `cosign attest`.
const slsaProvenancePredicateType = "slsaprovenance1"

// CosignGate verifies image signatures, and — when a stack's policy
// requires it — a SLSA provenance attestation, via cosign. A signature (or
// attestation) may satisfy the policy either by matching one of its static
// keys or one of its keyless Fulcio/Rekor identities; keys are tried
// first since they need no network round trip.
//
// Every failure mode — bad image reference, unreachable registry, no
// matching signature, missing attestation — becomes a rejected Verdict.
// CosignGate.Verify never returns a non-nil error for a per-image problem;
// that is the fail-closed contract Gate documents.
type CosignGate struct {
	policy    Policy
	keychain  authn.Keychain
	verifiers []signature.Verifier // one per policy.Keys, same order

	// trustedMu guards trusted/trustedOK rather than sync.Once: a fetch
	// that times out should be retried on the next call, not cached as a
	// permanent failure — a transient network blip must not permanently
	// disable keyless verification for the rest of this gate's lifetime.
	// Only a genuinely successful fetch is durable.
	trustedMu sync.Mutex
	trusted   root.TrustedMaterial
	trustedOK bool

	// fetchTrustedRootFn and trustedRootTimeout are test seams; NewCosignGate
	// sets them to cosign.TrustedRoot and trustedRootTimeout respectively.
	fetchTrustedRootFn func() (root.TrustedMaterial, error)
	trustedRootTimeout time.Duration
}

var _ Gate = (*CosignGate)(nil)

// NewCosignGate loads every static key in p and builds the verifiers
// CosignGate needs at Verify time, so a bad policy key file is caught at
// startup rather than on the first reconcile cycle. Keyless identity
// verification instead loads its trust material lazily on first use,
// since fetching the Sigstore trusted root is a network call and p may
// specify no identities at all.
func NewCosignGate(p Policy, authFile string) (*CosignGate, error) {
	kc, err := spec.Keychain(authFile)
	if err != nil {
		return nil, fmt.Errorf("gate: %w", err)
	}
	verifiers := make([]signature.Verifier, len(p.Keys))
	for i, k := range p.Keys {
		v, err := loadKeyVerifier(k.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("gate: policy key %d (%s): %w", i, k.KeyFile, err)
		}
		verifiers[i] = v
	}
	return &CosignGate{
		policy: p, keychain: kc, verifiers: verifiers,
		fetchTrustedRootFn: cosign.TrustedRoot,
		trustedRootTimeout: trustedRootTimeout,
	}, nil
}

func loadKeyVerifier(path string) (signature.Verifier, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pub, err := cryptoutils.UnmarshalPEMToPublicKey(pemBytes)
	if err != nil {
		return nil, err
	}
	return signature.LoadVerifier(pub, crypto.SHA256)
}

func (g *CosignGate) Verify(ctx context.Context, changes []apply.Change) ([]Verdict, error) {
	verdicts := make([]Verdict, len(changes))
	for i, c := range changes {
		verdicts[i] = g.verifyOne(ctx, c)
	}
	return verdicts, nil
}

func (g *CosignGate) verifyOne(ctx context.Context, c apply.Change) Verdict {
	v := Verdict{Service: c.Spec.Name, Image: c.Spec.Image}

	ref, err := name.ParseReference(c.Spec.Image)
	if err != nil {
		v.Reason = fmt.Sprintf("parse image reference: %v", err)
		return v
	}
	if err := g.verifySignature(ctx, ref); err != nil {
		v.Reason = err.Error()
		return v
	}
	if g.policy.RequireAttestationFor(c.Spec.Labels[spec.StackLabel]) {
		ok, err := g.hasValidAttestation(ctx, ref)
		if err != nil {
			v.Reason = fmt.Sprintf("load sigstore trusted root: %v", err)
			return v
		}
		if !ok {
			v.Reason = "attestation required"
			return v
		}
	}
	v.Pass = true
	return v
}

// verifySignature tries every static key before falling back to keyless
// identities, returning nil on the first match. A non-nil error joins every
// path's failure reason, since none of them alone explains why the image is
// untrusted.
//
// It calls cosign.VerifyImageAttestations, not VerifyImageSignatures: the
// pipeline signs with cosign's new (OCI-referrers-based) bundle format —
// the only format its `cosign sign`/`attest` write as of v3.1.1 — and in
// that format a plain signature is itself a DSSE-wrapped attestation with
// a generic predicate type. cosign's own CLI verify path takes the same
// route for exactly this reason; VerifyImageSignatures with
// CheckOpts.NewBundleFormat set is an explicit unimplemented stub in this
// version. See docs/contentions.md.
func (g *CosignGate) verifySignature(ctx context.Context, ref name.Reference) error {
	var errs []string
	for i, co := range g.keyCheckOpts() {
		atts, err := verifyBundle(ctx, ref, co)
		if err == nil && len(atts) > 0 {
			return nil
		}
		errs = append(errs, fmt.Sprintf("key %s: %v", g.policy.Keys[i].KeyFile, signatureError(atts, err)))
	}
	if co, err := g.identityCheckOpts(); err != nil {
		if len(g.policy.Identities) > 0 {
			errs = append(errs, fmt.Sprintf("load sigstore trusted root: %v", err))
		}
	} else if co != nil {
		atts, err := verifyBundle(ctx, ref, co)
		if err == nil && len(atts) > 0 {
			return nil
		}
		errs = append(errs, fmt.Sprintf("identities: %v", signatureError(atts, err)))
	}
	return errors.New(strings.Join(errs, "; "))
}

func signatureError(atts []oci.Signature, err error) error {
	if err != nil {
		return err
	}
	return errors.New("no signature found")
}

// hasValidAttestation reports whether ref carries an attestation that both
// verifies against the policy (key or identity) and matches the required
// SLSA provenance predicate type. Like verifySignature's identity branch,
// a non-nil error means the identity path's Sigstore trusted-root fetch
// failed — infrastructure unreachable, not "no attestation found" — so
// verifyOne can surface that distinctly instead of the misleading generic
// "attestation required" reason, which would send an operator hunting for
// a missing attestation that actually exists.
func (g *CosignGate) hasValidAttestation(ctx context.Context, ref name.Reference) (bool, error) {
	for _, co := range g.keyCheckOpts() {
		if g.matchesPredicateType(ctx, ref, co) {
			return true, nil
		}
	}
	co, err := g.identityCheckOpts()
	if err != nil {
		if len(g.policy.Identities) > 0 {
			return false, err
		}
		return false, nil
	}
	if co != nil && g.matchesPredicateType(ctx, ref, co) {
		return true, nil
	}
	return false, nil
}

// matchesPredicateType reports whether any attestation verified under co
// carries the SLSA v1 provenance predicate type specifically (as opposed
// to verifySignature's use of the same underlying call, which accepts any
// predicate type as proof the image is signed) AND names g.policy.BuilderID
// as its builder. Predicate-type matching alone only proves an attestation
// of the right shape exists; without also checking who claims to have
// produced it, a signed but vacuous or misleading provenance body would
// satisfy require_attestation just as well as a real one.
func (g *CosignGate) matchesPredicateType(ctx context.Context, ref name.Reference, co *cosign.CheckOpts) bool {
	atts, err := verifyBundle(ctx, ref, co)
	if err != nil {
		return false
	}
	for _, a := range atts {
		payload, _, err := policy.AttestationToPayloadJSON(ctx, slsaProvenancePredicateType, a)
		if err == nil && len(payload) > 0 && matchesBuilderID(payload, g.policy.BuilderID) {
			return true
		}
	}
	return false
}

// matchesBuilderID reports whether payload — an in-toto statement JSON with
// a SLSA v1 provenance predicate, as returned by
// policy.AttestationToPayloadJSON — names builderID as its
// predicate.runDetails.builder.id. A missing or empty builder id never
// matches, even against a blank builderID: LoadPolicy already refuses to
// leave BuilderID empty when attestation is required, so an empty policy
// value here only ever means "not applicable," not "unconstrained."
func matchesBuilderID(payload []byte, builderID string) bool {
	var stmt struct {
		Predicate struct {
			RunDetails struct {
				Builder struct {
					ID string `json:"id"`
				} `json:"builder"`
			} `json:"runDetails"`
		} `json:"predicate"`
	}
	if err := json.Unmarshal(payload, &stmt); err != nil {
		return false
	}
	id := stmt.Predicate.RunDetails.Builder.ID
	return id != "" && id == builderID
}

// keyCheckOpts returns one CheckOpts per policy.Keys entry, in order.
func (g *CosignGate) keyCheckOpts() []*cosign.CheckOpts {
	opts := make([]*cosign.CheckOpts, len(g.verifiers))
	for i, v := range g.verifiers {
		opts[i] = &cosign.CheckOpts{
			SigVerifier:        v,
			IgnoreTlog:         true,
			NewBundleFormat:    true,
			RegistryClientOpts: g.remoteOpts(),
		}
	}
	return opts
}

// identityCheckOpts returns the keyless-verification CheckOpts, or (nil,
// nil) when the policy has no identities to check. A non-nil error means
// the Sigstore trusted root couldn't be loaded.
func (g *CosignGate) identityCheckOpts() (*cosign.CheckOpts, error) {
	if len(g.policy.Identities) == 0 {
		return nil, nil
	}
	trusted, err := g.trustedRoot()
	if err != nil {
		return nil, err
	}
	return &cosign.CheckOpts{
		TrustedMaterial:    trusted,
		Identities:         identityMatchers(g.policy.Identities),
		NewBundleFormat:    true,
		RegistryClientOpts: g.remoteOpts(),
	}, nil
}

// verifyBundle wraps cosign.VerifyImageAttestations, discarding the
// bundle-verified bool verifySignature doesn't need.
func verifyBundle(ctx context.Context, ref name.Reference, co *cosign.CheckOpts) ([]oci.Signature, error) {
	atts, _, err := cosign.VerifyImageAttestations(ctx, ref, co)
	return atts, err
}

// trustedRoot fetches the Sigstore public trusted root on first use and
// caches a successful result for the gate's lifetime — every keyless
// verification in a reconcile cycle needs the same material, and
// re-fetching it per image would mean unnecessary network calls in the
// hot path. A failed or timed-out fetch is not cached, so a transient
// network problem gets retried on the next call instead of permanently
// disabling keyless verification.
func (g *CosignGate) trustedRoot() (root.TrustedMaterial, error) {
	g.trustedMu.Lock()
	defer g.trustedMu.Unlock()
	if g.trustedOK {
		return g.trusted, nil
	}
	trusted, err := g.fetchTrustedRootWithTimeout()
	if err != nil {
		return nil, err
	}
	g.trusted, g.trustedOK = trusted, true
	return g.trusted, nil
}

// fetchTrustedRootWithTimeout calls fetchTrustedRootFn (cosign.TrustedRoot
// in production) with a timeout it doesn't natively support, by racing it
// against a timer in its own goroutine.
func (g *CosignGate) fetchTrustedRootWithTimeout() (root.TrustedMaterial, error) {
	type result struct {
		trusted root.TrustedMaterial
		err     error
	}
	done := make(chan result, 1)
	go func() {
		trusted, err := g.fetchTrustedRootFn()
		done <- result{trusted, err}
	}()
	select {
	case r := <-done:
		return r.trusted, r.err
	case <-time.After(g.trustedRootTimeout):
		return nil, fmt.Errorf("fetch sigstore trusted root: timed out after %s", g.trustedRootTimeout)
	}
}

func (g *CosignGate) remoteOpts() []ociremote.Option {
	return []ociremote.Option{ociremote.WithRemoteOptions(remote.WithAuthFromKeychain(g.keychain))}
}

func identityMatchers(ids []Identity) []cosign.Identity {
	out := make([]cosign.Identity, len(ids))
	for i, id := range ids {
		out[i] = cosign.Identity{Issuer: id.Issuer, SubjectRegExp: id.SubjectRegex}
	}
	return out
}
