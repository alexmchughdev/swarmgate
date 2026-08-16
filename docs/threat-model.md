# Threat model

swarmgate's trust boundaries, considered threat vectors, what was fixed,
what was investigated and found not applicable, and what's a documented
limitation rather than a code fix. This is the record for the production
hardening pass covering commits `64b6e2b`..`96edb87` on `dev`, plus the
SLSA builder-id follow-up below.

## Trust boundaries

swarmgate sits between three parties with different levels of trust, and
most of the findings below come from one of them reaching further than it
should:

1. **The Git repository (`git.url`).** Whoever can push a stack file to the
   configured branch controls everything swarmgate deploys: image
   references, replica counts, ports, networks, environment variables. This
   is the intended control surface (it's GitOps), but it means a stack file
   is the most powerful untrusted input swarmgate accepts — treat "anyone
   who can push a commit" as the attacker for most of this document.
2. **The container registry.** Image content itself is out of scope (that's
   what the cosign gate is for); but the registry is also where signatures,
   attestations, and the Sigstore trusted root are fetched from, all over
   the network, all on a schedule swarmgate doesn't control.
3. **The Docker Engine / Swarm API.** swarmgate holds a live connection with
   full manager-level privilege. Everything the gate approves gets applied
   here with no further check.

A stack file is written by someone in boundary 1 but executed against
boundary 3 with no human in the loop — that gap is the reason the gate and
the parsing-time validation in `internal/spec` exist at all.

## Findings fixed this pass

### HIGH

- **Policy bypass on a blank issuer or subject_regex.** cosign treats an
  empty `Identity.Issuer` or `SubjectRegExp` as "match anything," so a
  policy author who left either field blank (a likely typo, not malicious
  intent) got silent unconstrained trust instead of a validation error.
  Fixed in `64b6e2b`: `LoadPolicy` now rejects a blank issuer or
  subject_regex outright.
- **Cross-stack qualified service-name collision.** `ServiceName(stack,
  service)` joins with an unescaped `_`, so stack `payments` service
  `db_writer` and stack `payments_db` service `writer` both qualify to
  `payments_db_writer`. Whichever stack file parsed second silently
  overwrote the first's service in the shared map — a real privilege
  escalation if the two stack files are owned by differently-trusted
  parties (e.g. per-team subdirectories with different push access). Fixed
  in `370121d` with an explicit collision tracker.
- **Compose interpolation exposed the full host environment.** `${VAR}`
  interpolation was resolved against `os.Environ()` — swarmgate's entire
  process environment, secrets included — rather than anything scoped to
  the stack file's own trust level. Anyone who could push a stack file
  could read back any environment variable swarmgate's process had, by
  interpolating it into a label or image tag and watching it appear in the
  observed cluster state. Fixed in `61e2a80`: interpolation now resolves
  only against `git.interpolation_vars`, an explicit operator-controlled
  allowlist, empty by default.
- **No panic recovery anywhere.** A panic in the reconcile cycle or in
  either background event-watching goroutine took the whole process down —
  a single malformed API response or an unanticipated nil somewhere becomes
  a full outage instead of one failed cycle. Fixed in `1b297f6`: the
  reconcile cycle and both event-stream goroutines now recover and degrade
  (failed cycle / reconnect-after-backoff) instead of crashing.
- **No timeout on any reconcile stage.** Fetch, resolve, observe, verify,
  and apply all ran under the process's own lifetime context, which carries
  no deadline. A hung git remote, registry, or Docker socket call blocked
  the whole cycle, and therefore every stack this instance manages,
  indefinitely. Fixed in `a8cff17` with a new `stage_timeout` config knob
  wrapping every stage; also surfaced (in the same investigation) a real
  `time.NewTicker` panic on `poll_interval: 0`, fixed with a floor
  validation on all three duration knobs.
- **Unbounded Sigstore trusted-root fetch.** `cosign.TrustedRoot()` accepts
  no context and its own HTTP client has no timeout; a slow or
  interfered-with Sigstore network path hung that call, and every keyless
  verification waiting on it, forever. Fixed in `4200f96` by racing the
  fetch against a timer in its own goroutine. The first fix attempt used
  `sync.Once`-style caching (matching the pre-existing pattern) and was
  caught and corrected before committing: caching a *timeout* the same way
  a success is cached would have turned one transient network blip into a
  permanent, silent loss of keyless verification for the rest of the
  process's life. The shipped version only caches a genuinely successful
  fetch.
- **Unbounded git clone/fetch and unbounded per-file size.** The in-memory
  clone requested full history with no object-count or size limit, and a
  single stack file's blob was read entirely into memory with no size cap
  before Parse ever saw it — either one lets a compromised or careless
  remote balloon swarmgate's memory use. Fixed in `11c1d57`: clone/fetch
  now request depth 1 (stack files are only ever read at branch head, so
  history depth bought nothing), and each stack file is capped at 1 MiB,
  checked against both the blob's declared size and an independently
  bounded read (the declared size comes from the object's own header and
  isn't trusted alone).
- **Git URL credentials could leak into error messages.** `git.url` can
  carry embedded credentials (`https://user:pass@host/...`, or a bare token
  used as the username in the scp-like `token@host:path` form). Wrapped
  clone/fetch errors interpolated the URL verbatim, risking a credential
  landing in a log line. Fixed in `11c1d57` with a redaction helper applied
  before the URL reaches any error message. `net/url.URL.Redacted()` was
  considered and rejected: it only masks the password half of userinfo (a
  bare-token-as-username leaks in full), and it panics outright on the
  scp-like syntax this codebase's `ssh_key_file` config implies is a
  supported form — both confirmed by direct experiment, not assumed.
- **Unbounded replicas/ports/networks/env per service.** Nothing in the
  compose spec bounds `deploy.replicas`, port list length, network
  attachment count, or environment variable count; a stack file author
  could ask the Docker API to scale a service to an arbitrary number of
  tasks (or port bindings, or network attachments) on the next reconcile
  cycle. Fixed in `95c2a90` with fixed sanity limits (1000 replicas, 100
  ports, 50 networks, 1000 env vars) enforced at parse time, before a
  desired-state change is ever diffed or applied.

### MEDIUM

- **Telemetry file world/group-readable.** Opened at mode 0644; events
  include image references, service names, and gate-rejection reasons —
  operational detail with no reason to be readable by other users on a
  shared host. Fixed in `765d54b` (mode 0600).
- **No permission check on secret files.** `git.ssh_key_file` (an SSH
  private key) and `registry.auth_file` (a docker `config.json`, which can
  hold registry passwords/tokens) were read straight off disk with no check
  that their mode actually restricted access to the owner. Fixed in
  `d247e2d` with a new `internal/secretfile` package, checked before either
  file is used; a group- or world-readable secret file now fails startup
  loudly instead of being trusted silently.
- **SLSA attestation content wasn't verified beyond predicate-type
  matching.** `matchesPredicateType` confirmed an attestation carried the
  `slsaprovenance1` predicate type and passed cosign's own signature/identity
  verification, but never inspected the provenance payload's contents — a
  signed-but-vacuous attestation (correct predicate type, minimal or
  misleading provenance body) satisfied `require_attestation` just as well
  as a real one. Fixed in `aeed582` with the smallest useful version of a
  content check: a new required `builder_id` policy field, checked against
  the payload's `predicate.runDetails.builder.id` in `matchesBuilderID`
  (`internal/gate/cosign.go`); `LoadPolicy` refuses a policy that requires
  attestation (globally or via `per_stack`) without one. This is
  deliberately narrow — it doesn't verify materials, invocation parameters,
  or anything else in the provenance body, and organization-specific
  provenance policy (dedicated tooling like slsa-framework's
  `slsa-verifier` or in-toto's `witness`/`policy`) is still out of scope —
  but it closes the specific "any signed attestation of the right shape
  passes regardless of who produced it" gap. Verified against a real
  cosign-signed attestation, not just hand-crafted JSON:
  `TestIntegrationOkAttestedRejectsWrongBuilderID`
  (`internal/gate/cosign_integration_test.go`) reuses the pipeline's actual
  `ok-attested` image and rejects it under a policy naming a builder id its
  real attestation doesn't carry.

## Investigated, found not applicable

- **"`IgnoreTlog: true` breaks keyless verification's certificate-validity
  check."** A review pass flagged that every `cosign.CheckOpts`
  construction in `internal/gate/cosign.go` sets `IgnoreTlog: true`, which
  would make cosign validate a Fulcio certificate's short validity window
  against wall-clock verification time instead of Rekor's signing-time
  timestamp — a real bug if true, since any check running even seconds
  after cert issuance would see an "expired" certificate.

  Direct inspection of the current code shows this doesn't hold:
  `keyCheckOpts()` (the static-key path) sets `IgnoreTlog: true`, which is
  correct there — static keys have no certificate validity window to
  validate in the first place. `identityCheckOpts()` (the keyless path the
  finding was actually about) never touches the field, so it defaults to
  Go's zero value, `false`. Confirmed against the vendored cosign v3.1.1
  source (`pkg/cosign/verify.go`): the `!co.IgnoreTlog` branch — the one
  the identity path actually takes — appends `verify.WithTransparencyLog(1)`
  and `verify.WithIntegratedTimestamps(1)`, i.e. the correct signing-time
  check. The `verify.WithCurrentTime()` branch the finding described only
  fires under `co.IgnoreTlog && !co.UseSignedTimestamps`, which requires
  `IgnoreTlog == true` — never the case for the identity path. No fix
  needed; logged here per the standing "verify every claim independently"
  discipline rather than silently dropped.

## Accepted limitations (documented, not fixed)

These were considered and deliberately left as operational guidance rather
than code changes — either because "fixing" them would mean re-deriving
functionality cosign/Sigstore already own, or because the cost of a general
fix isn't justified by the actual exposure.

- **`AwaitConverged` issues one Docker API call per target, sequentially.**
  Correct but not batched; a cycle touching many services pays one round
  trip per service rather than a single bulk query. This is a scale/latency
  concern, not a correctness or security one — worth revisiting if a
  cluster's per-cycle service count grows large enough for the sequential
  cost to dominate cycle time, but not before.
- **Key rotation requires a full process restart.** Static cosign keys are
  loaded once at `NewCosignGate` construction; rotating `gate.policy_file`'s
  key material takes effect only on the next process start, not on the
  next `policy_file` read. Acceptable for now given swarmgate itself
  already restarts on any config change; worth a SIGHUP-triggered reload if
  operational experience shows key rotation needs to happen without a
  deploy.
- **Telemetry event fields have no size cap.** `Event.Fields` is
  `map[string]any` with no per-field or per-event size limit; an unusually
  long error message or reason string could bloat the JSONL file faster
  than expected. Bounded indirectly by the new 1 MiB stack-file cap and by
  error messages generally being short and code-generated rather than
  attacker-authored free text, so the practical exposure is low; not worth
  the complexity of a generic truncation scheme unless real telemetry
  volume shows otherwise.
- **Docker socket privilege footprint.** swarmgate needs full Docker Engine
  manager API access to observe and apply swarm state — there's no scoped
  credential short of that. This is a deployment-topology concern, not a
  code fix: run swarmgate on a manager node with the socket mounted
  read-write only into swarmgate's own container/process, never shared with
  other workloads, and treat compromise of the swarmgate process as
  equivalent to compromise of swarm manager access.
- **cosign `key_file` entries are public keys, not secrets.** Considered
  alongside the SSH-key/auth-file permission check above and deliberately
  excluded: `gate.policy_file`'s `key_file` points at the *public* half of a
  cosign key pair (verification only), confirmed against
  `internal/gate/cosign_test.go`'s own fixture helper, which writes only the
  public key). A restrictive file mode on a public key protects nothing;
  checking it would just be friction with no security benefit.

## Out of scope

- Compromise of the Git remote's or registry's own access control (who can
  push, who can push signed images) is assumed to be handled by those
  systems, not by swarmgate.
- The Sigstore/Fulcio/Rekor trust chain itself (TUF root compromise, CT log
  split-view attacks) is Sigstore's threat model, not swarmgate's; swarmgate
  only bounds how long it will wait on that infrastructure, per the
  trusted-root timeout fix above.
