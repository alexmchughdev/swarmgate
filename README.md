# swarmgate

Declarative GitOps deployment for Docker Swarm with supply-chain
enforcement: a poll → parse → resolve → observe → diff → verify → apply →
converge reconcile loop that deploys only what a Git repository declares,
pinned by image digest, optionally gated on cosign signature/attestation
policy before anything is applied.

## Status

Pre-alpha, actively built. What that means concretely:

- The reconcile loop (poll/parse/resolve/observe/diff/apply/converge),
  drift attribution, event-driven wake, pruning, and the supply-chain gate
  (static-key and keyless signature verification, SLSA attestation
  requirements, per-service and abort-cycle modes) are implemented and
  covered by unit and integration tests — `make check` is green.
- Every major piece has also been manually verified against a **real**
  Docker Swarm cluster and a **real** cosign-signed image pipeline, not
  just unit tests. The Quickstart below is one of those verified paths.
- `swarmgate status` and the backoff/resilience behavior are the most
  recently landed pieces; give them a harder look before relying on them
  under load.
- No release binaries are published yet (`make release` exists; nothing has
  been tagged).

## Quickstart

Requires Go 1.26+ and a Docker Engine in Swarm mode (`docker swarm init`
if it isn't already).

```sh
make build   # dist/swarmgate, dist/harness
```

swarmgate reconciles a Docker Swarm cluster against `*.yaml` compose-style
stack files in a Git repository. This walks through pointing it at a
throwaway local repo and watching it deploy nginx.

```sh
# 1. A bare repo standing in for your Git remote, plus a working clone.
mkdir -p /tmp/swarmgate-quickstart && cd /tmp/swarmgate-quickstart
git init --bare bare.git
git clone bare.git clone
cd clone && git config user.name demo && git config user.email demo@example.com
git checkout -b main && touch .gitkeep && git add .gitkeep
git commit -m init && git push -u origin main
cd ..

# 2. Config. See "Configuration reference" below for every field.
cat > swarmgate.yaml <<'EOF'
git:
  url: /tmp/swarmgate-quickstart/bare.git
  branch: main
  path: "."
poll_interval: 5s
telemetry:
  out: /tmp/swarmgate-quickstart/events.jsonl
EOF

# 3. Run swarmgate against it (foreground; Ctrl-C to stop).
/path/to/dist/swarmgate --config swarmgate.yaml
```

In a second terminal, declare a service and push it:

```sh
cat > clone/demo.yaml <<'EOF'
services:
  web:
    image: nginx:1.27-alpine
    deploy:
      replicas: 1
EOF
cd clone && git add demo.yaml && git commit -m "add web" && git push
```

Within `poll_interval`, swarmgate creates `demo_web`, pins it to a digest,
and converges it. Watch it happen either in the first terminal's logs or by
tailing the telemetry file:

```sh
tail -f /tmp/swarmgate-quickstart/events.jsonl
```

or, once it's settled:

```sh
/path/to/dist/swarmgate status --config swarmgate.yaml
```
```
NAME     STATE      DIGEST        LAST_RECONCILE        COMMIT
demo_web  converged  <digest>      2026-...              <commit sha>
```

## Build

```sh
make check    # gofmt + vet + tests
make build    # dist/swarmgate, dist/harness
```

## Configuration reference

Mirrors `swarmgate.example.yaml` (the single source of truth — if the two
ever disagree, the example file is right):

```yaml
# swarmgate.yaml
git:
  url: "ssh://git@host/repo.git"     # required
  branch: "main"                      # default "main"
  path: "stacks/"                     # default "." — dir containing *.yaml stack files
  ssh_key_file: "/etc/swarmgate/key"  # optional; empty = default ssh agent/https
                                       # (must be mode 0600 — swarmgate refuses
                                       # a group- or world-readable key file)
  interpolation_vars: []              # names (not values) of swarmgate's own env
                                       # vars that stack files may reference via
                                       # ${VAR}; empty by default. Anyone who can
                                       # push a stack file can read back whatever
                                       # a listed variable resolves to — only add
                                       # names every stack-file author is meant
                                       # to see the value of.
env_file_root: "/datavol/env"         # optional host directory for service env_file paths
volume_bind_roots: []                 # absolute host roots allowed for bind-mount sources
poll_interval: "30s"                  # Go duration, default 30s
events:
  wake: true                          # default true
prune: true                           # default true
registry:
  auth_file: ""                       # optional path to docker config.json
                                       # (must also be mode 0600)
gate:
  enabled: false                      # default false
  mode: "per-service"                 # "per-service" | "abort-cycle"
  policy_file: ""                     # required iff enabled
telemetry:
  out: "/var/lib/swarmgate/events.jsonl"  # default "./events.jsonl"
docker:
  host: ""                            # default: environment / socket
converge_timeout: "5m"                # default 5m
stage_timeout: "30s"                  # default 30s; bounds each individual
                                       # git/registry/Docker API call before
                                       # apply, and each apply call — a stuck
                                       # remote can't hang the whole cycle
```

Relative service `env_file` entries are resolved under `env_file_root` on the
swarmgate host (for example, `env_file: web.env` reads
`/datavol/env/web.env`). Absolute entries are also accepted when they resolve
inside that root. The root is optional; stacks that use `env_file` are rejected
with a configuration-specific error when it is unset. Resolved files must
remain inside the configured root, including through symlinks.

Bind-mount volume sources are disabled unless `volume_bind_roots` lists one
or more host directories. Sources must be absolute and resolve inside one of
those roots. Unreferenced Swarm configs and secrets are retained after a
service replacement; cleanup remains an operator-managed task.

Top-level configs may use Compose `file:` declarations. Those paths are
relative to the stack file in the Git repository, and their bytes are read
from the same commit as the stack. Swarm configs are immutable, so change the
config object's name (the `_vN` convention) whenever its content changes;
existing objects with a declared name are treated as already correct without
content comparison. Secrets remain external-only.

Every field also has a `SWARMGATE_*` environment override — see
`internal/config/config.go`'s `envOverrides` table for the exact names.

## Supply-chain gate

When `gate.enabled` is true, every change is verified against
`gate.policy_file` (static cosign keys and/or keyless Fulcio/Rekor
identities, optionally requiring a SLSA v1 provenance attestation) before
it's applied — see `internal/gate` and `pipeline/README.md` for the
fixture image pipeline used to test it, and `scripts/README.md`'s
`harness verify` section for gate-specific scenarios (signed, unsigned,
wrong key, missing attestation, and a registry-side tag-swap race).

```yaml
# gate-policy.yaml
keys:
  - key_file: /etc/swarmgate/cosign.pub
require_attestation: false
builder_id: ""                        # required iff require_attestation is
                                       # true anywhere (globally or via
                                       # per_stack): the exact SLSA
                                       # provenance builder identity
                                       # (predicate.runDetails.builder.id)
                                       # an attestation must name. Matching
                                       # the predicate type alone only
                                       # proves an attestation of the right
                                       # shape exists, not that it came from
                                       # a builder this policy trusts.
```

## Docs

- `scripts/README.md` — every `scripts/` and `harness` tool.
- `pipeline/README.md` — the signed/unsigned test image fixtures.
- `analysis/README.md` — turning harness output into summary tables.
