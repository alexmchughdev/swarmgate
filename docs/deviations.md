# Deviations

Deliberate departures from the plan, with the reason each was forced.
Underspecified areas the plan simply never addressed (e.g. the gate
interface's exact shape, or `status`'s output format) are architectural
decisions, not deviations, and are logged in `docs/contentions.md` instead.

## 2026-07-06 — converged clause (d): task health folded into clause (c)

The plan specifies a distinct convergence clause (d) inspecting per-task
container health status. The Swarm API exposes no health field on
`swarm.TaskStatus`, so that clause cannot be implemented as written. The
engine already gates task state on health: a task whose container defines a
healthcheck is held in `starting` until the healthcheck passes, and only then
reports `running`. Clause (c) counts only `running` tasks, so a service that
satisfies (c) has by construction passed its healthchecks. Clause (d) is
therefore folded into (c); see the `DEVIATION` comment in
`internal/observe/converged.go`.

## 2026-07-12 — T2 image-drift retargets to a fixed tag, not "the previous tag"

The plan's image-drift case retags a service to "the previous pinned tag."
swarmgate always deploys digest-pinned images (`repo@sha256:...`), never a
bare tag, so there is no reliable way to inspect a live service's current
digest and reverse-map it to "the entry before this one in the harness's own
tag-cycle history" — that mapping only exists inside the harness process,
not anywhere the engine or observed state can report it back. `t2RollbackImage`
retargets unconditionally to the first entry in `t1Tags` instead: a fixed,
deterministic rollback target standing in for "some previous pinned tag,"
sufficient to exercise detection and repair without claiming to reconstruct
real history. See the design note on `t2RollbackImage` in
`internal/harness/t2.go`.

## 2026-07-12 — T2 unmanaged-drift repair check uses a direct inspect, not converged-event absence

Every other T2 drift kind verifies repair by waiting for the next `converged`
event. That signal doesn't work for the unmanaged case: converged events fire
for any cycle that changed something, including one that only skipped a
removal (pruning disabled) — their presence proves nothing about whether the
unmanaged decoy specifically survived. `t2UnmanagedRepairRow` instead waits
out a fixed window and directly inspects the decoy, treating "still present"
as the correct outcome. See `internal/harness/t2.go`.

## 2026-07-12 — T4's `diff` and `window` offsets are implemented identically

The plan names `diff` and `window` as two distinct trigger points in the
race window between swarmgate's diff computation and its first apply call.
No telemetry event exists between those two points — `internal/loop/loop.go`
emits `diff`, then goes straight into the apply loop — so there is no finer
observable boundary to fire `window` on than the `diff` event itself. Both
offsets fire on the same event; see `t4ShouldFireOnEvent` in
`internal/harness/t4.go`.

## 2026-07-12 — T6's `--mode` is a label, not a control

The plan's `--mode per-service|abort-cycle` flag reads as something the
harness switches. `gate.mode` is swarmgate's own config, not something a
client observing its telemetry can change or query, so the harness cannot
configure the instance under test — `--mode` is folded into each row's
`Condition` for record-keeping only. Comparing the two modes' behavior means
running the same `--case` against two separately configured swarmgate
instances and diffing the resulting rows/JSONL by hand; documented as a
manual procedure in `scripts/README.md`'s `harness t6` section rather than
automated.
