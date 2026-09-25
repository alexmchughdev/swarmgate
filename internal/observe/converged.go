package observe

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"

	"github.com/alexmchughdev/swarmgate/internal/diff"
	"github.com/alexmchughdev/swarmgate/internal/spec"
)

// awaitTarget is one service AwaitConverged watches: either a desired spec
// that must be live, or a removal that must have disappeared.
type awaitTarget struct {
	name    string
	desired spec.ServiceSpec
	remove  bool
}

// AwaitConverged blocks until every service touched by d has converged on
// its desired state, polling at the observer's interval under an overall
// deadline of timeout. On deadline or cancellation the error names each
// unconverged service and the clause that failed for it, in sorted name
// order, so the caller can attach it to telemetry verbatim.
func (o *SwarmObserver) AwaitConverged(ctx context.Context, d diff.Diff, timeout time.Duration) error {
	targets := make([]awaitTarget, 0, len(d.Creates)+len(d.Updates)+len(d.Removes))
	for _, s := range d.Creates {
		targets = append(targets, awaitTarget{name: s.Name, desired: s})
	}
	for _, c := range d.Updates {
		targets = append(targets, awaitTarget{name: c.Name, desired: c.New})
	}
	for _, s := range d.Removes {
		targets = append(targets, awaitTarget{name: s.Name, remove: true})
	}
	if len(targets) == 0 {
		return nil
	}
	slices.SortFunc(targets, func(a, b awaitTarget) int { return strings.Compare(a.name, b.name) })

	interval := o.pollInterval
	if interval <= 0 {
		// A zero-value observer must degrade to the default cadence, not
		// panic inside time.NewTicker.
		interval = time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		pending := o.pollTargets(ctx, targets)
		if len(pending) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("await converged: %s: %w", strings.Join(pending, "; "), ctx.Err())
		case <-ticker.C:
		}
	}
}

// pollTargets evaluates every target once and returns one "name: detail"
// line per unconverged service, preserving the sorted target order.
func (o *SwarmObserver) pollTargets(ctx context.Context, targets []awaitTarget) []string {
	// Inspect returns the spec as submitted; swarmgate submits network
	// IDs (apply.attachByID), and the stored spec reads them back as IDs
	// too. The ID→name map translates those back to names so FromSwarm's
	// output compares against the desired side's names; a target absent
	// from the map passes through verbatim.
	var netNames map[string]string
	var netErr error
	for _, t := range targets {
		if !t.remove {
			netNames, netErr = o.networkNames(ctx)
			break
		}
	}

	var pending []string
	for _, t := range targets {
		if !t.remove && netErr != nil {
			pending = append(pending, fmt.Sprintf("%s: %v (clause a)", t.name, netErr))
			continue
		}
		if detail, ok := o.checkTarget(ctx, t, netNames); !ok {
			pending = append(pending, t.name+": "+detail)
		}
	}
	return pending
}

// networkNames maps network IDs to names for FromSwarm.
func (o *SwarmObserver) networkNames(ctx context.Context) (map[string]string, error) {
	networks, err := o.api.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	names := make(map[string]string, len(networks))
	for _, n := range networks {
		names[n.ID] = n.Name
	}
	return names, nil
}

// checkTarget evaluates one target and, when unconverged, says which clause
// failed. Clauses for creates/updates: (a) observed normal-form spec equals
// desired, (b) no update in flight, (c) desired count of running tasks on
// the desired image. Removes converge when inspect reports not-found.
func (o *SwarmObserver) checkTarget(ctx context.Context, t awaitTarget, netNames map[string]string) (string, bool) {
	svc, _, err := o.api.ServiceInspectWithRaw(ctx, t.name, swarm.ServiceInspectOptions{})

	if t.remove {
		switch {
		case err == nil:
			return "still present (remove)", false
		case cerrdefs.IsNotFound(err):
			return "", true
		default:
			return fmt.Sprintf("inspect: %v", err), false
		}
	}

	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return "service not found (clause a)", false
		}
		return fmt.Sprintf("inspect: %v", err), false
	}

	got := spec.FromSwarm(svc, netNames)
	for key := range got.DeployLabels {
		if _, wanted := t.desired.DeployLabels[key]; !wanted {
			delete(got.DeployLabels, key)
		}
	}
	if !reflect.DeepEqual(got, t.desired) {
		return "observed spec differs from desired (clause a)", false
	}

	if us := svc.UpdateStatus; us != nil && us.State != swarm.UpdateStateCompleted {
		return fmt.Sprintf("update %s (clause b)", us.State), false
	}

	tasks, err := o.api.TaskList(ctx, swarm.TaskListOptions{
		Filters: filters.NewArgs(filters.Arg("service", t.name)),
	})
	if err != nil {
		return fmt.Sprintf("list tasks: %v", err), false
	}
	var running uint64
	for _, task := range tasks {
		if task.DesiredState != swarm.TaskStateRunning || task.Status.State != swarm.TaskStateRunning {
			continue
		}
		if cs := task.Spec.ContainerSpec; cs == nil || cs.Image != t.desired.Image {
			continue
		}
		running++
	}
	if running != t.desired.Replicas {
		return fmt.Sprintf("tasks running %d/%d (clause c)", running, t.desired.Replicas), false
	}

	// DEVIATION: the plan calls for a separate health clause (d) reading
	// task-level health status, but the Swarm API exposes no health field on
	// TaskStatus. The engine keeps a healthchecked task in "starting" until
	// its healthcheck passes, so any task counted by clause (c) has already
	// passed its healthcheck; clause (d) is folded into (c).
	return "", true
}
