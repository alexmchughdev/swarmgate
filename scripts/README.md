# Scripts

## e2e-smoke.sh

Single-node end-to-end acceptance for the reconcile path. Not run in CI:
it needs a real Docker daemon with swarm mode and network access to Docker
Hub.

```sh
make build
./scripts/e2e-smoke.sh
```

What it does:

1. Initialises swarm mode if needed (and leaves it again on exit).
2. Creates a throwaway git repository containing one stack (`smoke`) with a
   single healthchecked nginx service.
3. Runs `swarmgate --once`: asserts the service exists, its image is
   digest-pinned, and a `converged` telemetry event was emitted.
4. Bumps the image tag in the repository and runs `swarmgate --once` again:
   asserts the deployed digest changed and a second `converged` event
   (with an `apply` event of action `update`) was emitted.
5. Prints the full telemetry JSONL and cleans up the service, network, and
   temporary state.

Any assertion failure exits non-zero with a `FAIL:` line.

## Harness pre-flight

Before running any scenario below:

- **No stray swarmgate processes, cluster state clean.** Exactly one
  `swarmgate` instance should be running on the host — the one under
  test. `harness` refuses to start (exit 2) if it finds more than one
  `swarmgate` process, since every extra instance polls and reconciles
  the same Docker daemon concurrently, corrupting measurements with
  unrelated apply/converge activity and API contention (see
  `internal/harness.CheckNoStraySwarmgate`; seven abandoned processes left
  running from unrelated demos once caused a real run to fail with Docker
  API timeouts). The check only catches extra *processes*; it cannot see
  whether the cluster itself still carries services or networks left over
  from an earlier, unrelated session — before a run whose results need to
  be trusted, check by hand:
  - `docker service ls` — must be empty (or contain only the instance
    under test's own services, if it's already deployed something).
  - `docker network ls` — no leftover `<stack>_default` overlay networks
    from an earlier session's stacks. An orphaned overlay network alone
    is harmless (nothing reconciles against it), but it's the same class
    of debris as the stray processes above and a sign the host wasn't
    actually clean going in.
- **`docker network rm` is not sufficient cleanup between runs sharing a
  cluster — roll back to a clean snapshot instead.** Docker/Moby can leak
  the kernel-level network namespace mount backing an overlay network past
  its removal from `docker network ls`, on every node that ever ran a
  task on it — confirmed surviving both `docker network rm` and a full
  daemon restart. A later network that happens to be auto-allocated the
  same subnet (common: on a lightly-used cluster there's usually only one
  overlay network live at allocation time, so Docker's IPAM hands out the
  same first-available range every time) then fails every attempt to
  attach a *new* container with `"invalid pool request: Pool overlaps
  with other one on this address space"` — a failure mode that only shows
  up for drift/update kinds whose repair allocates a fresh attachment
  (e.g. an image change) and not ones that don't (e.g. a pure replica
  scale-down), making it look like a per-scenario flake rather than the
  shared-cluster problem it actually is. Nothing in `docker network
  ls`/`docker service ls` reveals a leaked mount, so eyeballing cluster
  state clean is not sufficient evidence that it is. **Between any two
  runs sharing one cluster, roll every affected VM back to a known-clean
  Proxmox snapshot (`qm rollback` from `root@pve`) rather than
  hand-cleaning cluster state** — this is the only remediation confirmed
  reliable, and it's what makes results defensible against "was the
  cluster actually clean" in a way in-guest cleanup commands cannot.
- A real registry/cluster reachable from both the harness and the
  `swarmgate` instance under test, per each scenario's own requirements
  below.

## harness scale

Convergence-time run: each iteration bumps `--changes` service image tags
in a stack, commits (and by default pushes), then measures time from push
to the matching `converged` telemetry event.

Requires a running `swarmgate` instance reconciling `--repo`'s remote, with
its `telemetry.out` pointed at the file passed to `--events-file`.

```sh
make build
./dist/harness scale \
  --repo /path/to/working/clone \
  --stack scale \
  --scale 1 \
  --changes 1 \
  --events-file /path/to/events.jsonl \
  --out results.csv \
  --label events=on \
  -n 5
```

- `--scale` and `--changes` sweep independently across a series of runs
  (e.g. `--scale 10 --changes 1`, `--scale 50 --changes 5`) to trace
  convergence time against fleet size and change volume.
- `--label` records which `swarmgate` config produced the run (typically
  `events=on` or `events=off`, matching whether `events.wake` was enabled)
  — the harness does not control the daemon's config itself.
- `--push false` suits a local bare-remote setup where `git push` would be
  a same-host no-op; the commit alone is enough for `swarmgate` to poll.

## harness drift

Drift detect/repair run: each iteration applies one direct Docker SDK
mutation to a live service, bypassing git entirely, then measures time to
the matching `drift` event (detect) and, for four of the five kinds, the
following `converged` event (repair).

```sh
./dist/harness drift \
  --drift image \
  --service scale_web1 \
  --events-file /path/to/events.jsonl \
  --out results.csv \
  --label events=on \
  -n 30
```

- `--drift replicas|image|env|removed|unmanaged` selects the mutation kind
  (the same vocabulary as the `drift` telemetry event's `kind` field).
- `--service` is the full qualified service name (e.g. `scale_web1`); for
  `--drift unmanaged` it is instead the *base* name given to a freshly
  created decoy service (unique per run).
- `--drift unmanaged` only makes sense against a `swarmgate` instance
  configured with `prune: false` — the whole point is that the decoy is
  detected as drift but never removed. Running it against `prune: true`
  reports `error` rows (the decoy got pruned, which is a configuration
  mismatch, not a scenario failure).
- `--poll-interval` must match the target instance's configured
  `poll_interval`; it is used only for the `unmanaged` case's post-wait
  verification window.

## harness fault

Node/process fault injection: kills a manager, worker, or the reconciler
itself mid-cycle (via an opaque `ssh`/`sh -c` command from a hosts config),
awaits convergence, restores, and independently verifies the live cluster
against what was pushed (a believed-state false-ok check: did the
reconciler ever report `converged` while the live cluster had actually
diverged from what was pushed).

Requires SSH-reachable hosts distinct from where the harness runs — a
single-node dev machine cannot exercise this scenario meaningfully; see
`harness-hosts.example.yaml` at the repo root for the config shape.

```sh
cp harness-hosts.example.yaml my-hosts.yaml   # edit kill/restore commands for your cluster
./dist/harness fault \
  --hosts-config my-hosts.yaml \
  --target worker1 \
  --restore-after 60s \
  --repo /path/to/working/clone \
  --service web1 \
  --events-file /path/to/events.jsonl \
  --out results.csv \
  --label events=on \
  -n 10
```

- `--target` must be a key present in `--hosts-config`; the file's keys
  are the valid target set (not a fixed enum in code).
- `--restore-after 0` restores once at the end of the run regardless of
  outcome; a nonzero duration additionally schedules an early restore on
  its own timer, whichever fires first.
- Each run's convergence wait is fixed at 10 minutes (not a flag), per the
  scenario's own long-tail failure-recovery premise.
- `Detail` encodes `false_ok=true|false` (see above), `readout=event|poll`
  (see below), plus any `kill_error`/`restore_error`/`verify_error`
  segments.
- **The `reconciler` target (killing swarmgate's own process, not a VM)
  cannot rely on the `converged` telemetry event.** Kill fires on the
  `apply` event, which is emitted after the action it describes already
  succeeded — so the fault always lands on an already-converged commit, and
  swarmgate only emits `converged` for a cycle that changed something
  (`internal/loop/loop.go`). The restarted process's next poll finds an
  empty diff and never re-emits one. `faultRunner.run` special-cases this
  target: it polls live state directly (`readout=poll` in `Detail`) instead
  of waiting on the event stream (`readout=event`, every other target).
  This is a real gap in using telemetry to self-observe convergence, not a
  harness bug, and has implications for how convergence claims should be
  scoped when this data is used elsewhere.
- **Restoring a killed VM via the Proxmox API does not guarantee its Docker
  daemon comes back up.** `qm start`-equivalent restarts were observed
  leaving `docker` stopped despite being in the guest's OpenRC `default`
  runlevel. Any VM-kill restore command should explicitly wait for SSH and
  then `rc-service docker start` (idempotent) rather than trusting the
  boot sequence. A worker/manager target's recorded `duration_ms` reflects
  time to cluster-level recovery (Swarm rescheduling onto a surviving
  node), which can complete before the killed node itself has finished
  coming back — not "time for the killed node to rejoin."

## harness race

TOCTOU race: races a direct operator mutation (an out-of-band env set)
against swarmgate's own reconcile of a concurrent, legitimate git push to
the same service, firing the operator's change at a configurable point in
the cycle.

```sh
./dist/harness race \
  --offset apply \
  --service web1 \
  --repo /path/to/working/clone \
  --events-file /path/to/events.jsonl \
  --out results.csv \
  --label events=on \
  -n 20
```

- `--service` is the bare compose service key within the stack (e.g.
  `web1`), not stack-qualified — the harness qualifies it internally
  (`spec.ServiceName(stack, service)`, e.g. `race_web1` for the default
  `--stack race`) everywhere it needs to match a live service or a
  telemetry event's `Service` field.
- `--offset diff|window|apply|after` is the trigger point relative to the
  push's own reconcile cycle. `diff` and `window` fire at the same
  observable point (the `diff` telemetry event) — there is no finer
  boundary between "diff computed" and "apply about to start" than that
  event itself.
- `Detail` encodes `winner=git|operator;detected=true|false`, plus
  `;revert_ms=<n>` when the operator's change was detected and then
  reverted (`winner=git`). `detected=false` is the common, expected case
  for the `diff`/`window`/`apply` offsets, since the settling `converged`
  event for those usually arrives from the same cycle the push triggered,
  before a later poll could notice the operator's change as drift;
  `after` is the offset most likely to observe a full drift-then-revert
  sequence, since its own trigger is deferred until the first cycle's
  `converged` event.

## harness latency

Registry latency toggle: injects network delay or unreachability at a
remote registry host via `tc netem` / `iptables` over SSH, then runs
ordinary scale-style push-and-await samples under that condition.

Requires an SSH-reachable registry host distinct from where the harness
runs — a single-node dev machine cannot exercise this scenario
meaningfully.

```sh
./dist/harness latency \
  --latency 500ms \
  --registry-host user@registry-host \
  --iface eth0 \
  --repo /path/to/working/clone \
  --service web1 \
  --events-file /path/to/events.jsonl \
  --out results.csv \
  --label events=on \
  -n 10
```

- `--latency 0|500ms|5s|unreachable` — `0` is the control/baseline
  condition (no network condition is applied at all); `unreachable` drops
  inbound traffic to the registry's port (5000) via `iptables` instead of
  injecting delay.
- The condition is applied once per harness invocation (not once per run)
  and restored once at the end, always, regardless of how the `-n` runs
  went.
- `unreachable` runs should expect `timeout` outcomes — that is the
  scenario working as intended, not a harness failure.

## harness verify

Gate scenarios: points a stack service at one of the `pipeline/build.sh`
fixture images (see `pipeline/README.md`) and records the observed
verify/converge outcome. Requires `pipeline/build.sh --registry <host:port>`
to have already run against the registry — `verify` does not build or sign
anything itself, only reads what's already there — and a swarmgate instance
under test configured with `gate.enabled: true` and a `gate.policy_file`
trusting `pipeline/keys/cosign.pub`.

```sh
./dist/harness verify \
  --case ok \
  --registry localhost:5000 \
  --repo /path/to/working/clone \
  --service web1 \
  --events-file /path/to/events.jsonl \
  --out results.csv \
  --mode per-service \
  -n 5
```

- `--case unsigned|wrong-identity|no-attestation|ok|tag-repoint`:
  - `unsigned`/`wrong-identity` reject under any policy trusting
    `pipeline/keys/cosign.pub` (no signature at all; signed with a key the
    policy doesn't trust). Expect `outcome=ok;detail=verify=reject`.
  - `no-attestation` only rejects under a policy with
    `require_attestation: true` — against a policy with it `false` it's
    just a validly signed image and correctly passes, since it has no
    attestation requirement to fail. Run this case against a
    `require_attestation: true` policy specifically.
  - `ok` requires a policy that does **not** require attestation
    (`ok-signed` carries no attestation) and additionally awaits the
    matching `converged` event, so it also proves the image actually
    deploys and runs, not just that the gate passes it.
  - `tag-repoint` seeds a dedicated `tag-repoint:v1` tag with `ok-signed`'s
    content (image and signature bundle both — see below), waits for
    swarmgate's `resolve` event to capture the pinned digest, then retags
    `v1` onto the unsigned variant's digest and waits for convergence. It
    then inspects the actually-deployed service and asserts its digest
    still equals the one `resolve` captured — proving swarmgate deploys
    what it pinned and verified, not whatever the tag currently points to.
    Requires this case also run against a policy that does not require
    attestation (same seed image as `ok`), and `--docker-host` if the
    engine isn't reachable via the standard environment.
- `--mode` is recorded in `Condition` only — the harness has no way to
  configure gate.mode on the instance under test. Comparing per-service
  against abort-cycle behavior means running the same `--case` (e.g.
  `wrong-identity`, `-n 10`) against two separately configured swarmgate
  instances and diffing the resulting rows/JSONL by hand: per-service
  cycles show only the rejected service's `verify` event and continue
  normally; abort-cycle cycles show the same `verify` event plus an
  additional `error` event (`believed=aborted;reason=gate`) and apply
  nothing else that cycle either.

### Manually verified

Run against a single-node swarm with `pipeline/build.sh --registry
localhost:5000` already applied, a policy trusting `pipeline/keys/cosign.pub`
(`require_attestation: false` except where noted), `gate.mode: per-service`:

```
case=ok             outcome=ok      detail=verify=pass;converged=true
case=unsigned       outcome=ok      detail=verify=reject
case=wrong-identity outcome=ok      detail=verify=reject
case=no-attestation outcome=ok      detail=verify=reject   (require_attestation: true policy)
case=tag-repoint    outcome=ok      detail=resolved=sha256:48aa90fa...;deployed=sha256:48aa90fa...;match=true
```

The `tag-repoint` JSONL trail additionally shows the protection is not a
one-time race window: every poll after the retag re-resolves `v1` to the
unsigned digest, computes an image-drift update, and rejects it again —
the originally deployed (signed) digest never changes for as long as the
tag stays pointed at unsigned content.

## harness refconverge

ArgoCD/RKE2 quantitative reference, equivalent to the `scale` scenario:
reproduces `scale`'s scale/changes convergence-timing matrix against ArgoCD
instead of swarmgate, so the two can be compared on the same measurement
definition (harness-observed git push to the target platform's own
externally-visible healthy state). Renders a Kubernetes `Deployment`
manifest set instead of a compose stack; measures convergence via the
ArgoCD `Application` resource's own `.status.sync`/`.status.health`
fields, polled through `kubectl`, never ArgoCD's internal reconciliation
timestamps.

Requires an ArgoCD `Application` already created and pointed at `--stack`
(a fixed path within `--repo` — unlike swarmgate's `git.path`, an
Application's source path can't be varied per-run, so every condition in a
series of runs must share one `--stack`, sequentially, the same way
`scale`'s own `--stack` stays fixed across its scale/changes sweep).

```sh
./dist/harness refconverge \
  --repo /path/to/argocd-watched/clone \
  --stack refconverge \
  --app-name refconverge \
  --namespace argocd \
  --scale 10 \
  --changes 5 \
  --registry localhost:5000 \
  --image eval-workload \
  --tags v1,v2,v3 \
  --trigger-refresh \
  --out results.csv \
  --label scale-equivalent \
  -n 30
```

- `--trigger-refresh` annotates the Application for an immediate hard
  refresh right after each push, standing in for a webhook notification —
  without it, ArgoCD only notices a new commit on its own periodic
  reconciliation timer (minutes by default), which is fine for measuring
  the poll-only case specifically but impractical for a full run's
  wall-clock time otherwise.
- No `--events-file`: `refconverge` (and `refdrift`, below) poll ArgoCD's
  own status instead of tailing swarmgate telemetry, so `--events-file` is
  not required for these two scenarios specifically (every other scenario
  still requires it).

## harness refdrift

ArgoCD/RKE2 quantitative reference, equivalent to the `drift` scenario:
drift detection and repair against ArgoCD instead of swarmgate. Pushes a
fixed single-deployment baseline, injects one out-of-band drift directly
against the live cluster (bypassing git entirely, matching `drift`'s own
direct-Docker-API mutations), then polls the Application's status for
`OutOfSync` (detect) and back to `Synced`+`Healthy` (repair, via ArgoCD's
own `syncPolicy.automated.selfHeal` — already required on the Application
under test, the direct analogue of swarmgate's reconcile loop needing no
separate trigger to correct drift once noticed).

```sh
./dist/harness refdrift \
  --repo /path/to/argocd-watched/clone \
  --stack refconverge \
  --app-name refconverge \
  --namespace argocd \
  --drift replicas \
  --deployment web1 \
  --registry localhost:5000 \
  --image eval-workload \
  --trigger-refresh \
  --out results.csv \
  --label drift-equivalent \
  -n 30
```

- `--drift replicas|image|env|removed` — the four `drift` kinds with a
  direct native K8s equivalent (`kubectl scale`/`set image`/`set env`/
  `delete`, respectively). `drift`'s fifth kind, `unmanaged`, has no
  `refdrift` implementation: it tests something much closer to
  true-by-construction given how ArgoCD scopes "managed" (its own
  Application-tracked resources plus an auto-applied ownership label, not
  a broader always-on scan the way swarmgate's `swarmgate.managed=true`
  label works).
- Emits two rows per run (`detect`, `repair`), matching `drift`'s own
  row-pair shape exactly.
</content>
