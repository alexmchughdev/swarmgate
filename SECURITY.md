# Security

swarmgate is a supply-chain security tool: it reconciles a Docker Swarm
cluster against a Git source of truth and can gate every change on cosign
signature/SLSA attestation policy before applying it. Vulnerabilities here
have real consequences for anyone relying on it to enforce that gate or to
keep a cluster converged on trusted state — please report them
responsibly.

## Reporting a vulnerability

GitHub private vulnerability reporting is not yet enabled on this
repository. Until it is, report a vulnerability by emailing
alexanderedwardmchugh@gmail.com directly rather than opening a public
issue. Include enough detail to reproduce the issue (affected version or
commit, a stack file or config that triggers it, expected vs. actual
behavior).

## Scope

In scope:

- The reconciler (`internal/loop`, `internal/spec`, `internal/observe`,
  `internal/diff`, `internal/apply`, `internal/source`, `internal/config`,
  `internal/secretfile`, `internal/telemetry`, `cmd/swarmgate`)
- The supply-chain gate (`internal/gate`)
- The harness (`internal/harness`, `cmd/harness`)

Out of scope:

- `pipeline/` — deliberately insecure test fixtures (unsigned images,
  a throwaway signing key not trusted by any real policy, and similar) used
  to exercise the gate's own rejection paths. Nothing in that directory is
  meant to be secure, and reports about it will be closed as out of scope.

## Response time

No SLA is offered on response or fix time at this stage of the project.
