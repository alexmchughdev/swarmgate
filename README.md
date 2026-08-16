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
- Every major piece has been manually verified against a **real** Docker
  Swarm cluster and a **real** cosign-signed image pipeline, not just unit
  tests — see `docs/build-log.md` for the specific commands and captured
  output. The Quickstart below is one of those verified paths.
- A full multi-node evaluation campaign has since been run against a real
  4-node Swarm cluster (3 compute nodes + a private registry): convergence
  timing across a scale/changes matrix, drift detection and repair, fault
  injection (process/node kill across 4 targets), TOCTOU races against an
  out-of-band operator mutation, registry-latency sensitivity, and gate
  correctness under both `gate.mode`s — some 1,700+ measured runs in total,
  zero false positives across every fault-injection-style scenario. A
  separate ArgoCD/RKE2 reference campaign reproduces the convergence-timing
  and drift scenarios against a different GitOps controller for comparison.
- `swarmgate status` and the backoff/resilience behavior (C32–C33) are the
  most recently landed pieces; give them a harder look before relying on
  them under load.
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

### Reproducing a T1 convergence campaign

T1 measures push-to-converged latency by repeatedly bumping a service's
image tag. This is the exact command run to verify it (see
`docs/build-log.md`, Phase 3 gate) — it assumes the same setup as the
Quickstart above, with swarmgate already running against `swarmgate.yaml`:

```sh
./dist/harness t1 \
  --repo /tmp/swarmgate-quickstart/clone \
  --stack quickstart \
  --scale 1 \
  --changes 1 \
  --events-file /tmp/swarmgate-quickstart/events.jsonl \
  --out results.csv \
  -n 3
```

Real output from this exact command against a fresh clone:

```
scenario,condition,run,t_start,t_end,duration_ms,outcome,detail
t1,scale=1;changes=1;,1,2026-07-12T21:43:35.645518804Z,2026-07-12T21:43:40.103434Z,4457,ok,
t1,scale=1;changes=1;,2,2026-07-12T21:43:40.152017493Z,2026-07-12T21:43:53.898128Z,13746,ok,
t1,scale=1;changes=1;,3,2026-07-12T21:43:53.908498523Z,2026-07-12T21:44:05.603677Z,11695,ok,
```

`--push` defaults to `true`, which is correct here since `--repo` has a
real remote (`bare.git`) that swarmgate actually polls; only pass
`--push=false` if `--repo` has no remote at all (swarmgate would then
never see the harness's local-only commits). See `scripts/README.md` for
every harness scenario (t1–t6 against swarmgate itself; t8–t9 against
ArgoCD, for a quantitative reference point on the same measurements) and
`analysis/README.md` for turning a campaign's `results.csv` into a summary
table.

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

Every field also has a `SWARMGATE_*` environment override — see
`internal/config/config.go`'s `envOverrides` table for the exact names.

## Supply-chain gate

When `gate.enabled` is true, every change is verified against
`gate.policy_file` (static cosign keys and/or keyless Fulcio/Rekor
identities, optionally requiring a SLSA v1 provenance attestation) before
it's applied — see `internal/gate` and `pipeline/README.md` for the
fixture image pipeline used to test it, and `scripts/README.md`'s
`harness t6` section for gate-specific scenarios (signed, unsigned, wrong
key, missing attestation, and a registry-side tag-swap race).

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

- `docs/threat-model.md` — trust boundaries, what's been hardened against,
  and what's a documented limitation rather than a fix.
- `docs/deviations.md` — deliberate departures from the original plan,
  with the reason each was forced.
- `docs/build-log.md` / `docs/contentions.md` — the running build history
  and every architectural decision made along the way (untracked; ask if
  you want them committed).
- `scripts/README.md` — every `scripts/` and `harness` tool.
- `pipeline/README.md` — the signed/unsigned test image fixtures.
- `analysis/README.md` — turning harness campaign output into tables.
