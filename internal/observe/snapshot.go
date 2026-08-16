package observe

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"

	"github.com/alexmchughdev/swarmgate/internal/spec"
)

// Snapshot returns the current managed services in normal form. Filtering on
// the managed label happens server-side, so unmanaged services never enter
// the pipeline at all.
func (o *SwarmObserver) Snapshot(ctx context.Context) (spec.ObservedState, error) {
	services, err := o.api.ServiceList(ctx, swarm.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("label", spec.ManagedLabel+"=true")),
	})
	if err != nil {
		return spec.ObservedState{}, fmt.Errorf("list services: %w", err)
	}

	names, err := o.networkNames(ctx)
	if err != nil {
		return spec.ObservedState{}, err
	}

	state := spec.ObservedState{Services: make(map[string]spec.ServiceSpec, len(services))}
	for _, s := range services {
		svc := spec.FromSwarm(s, names)
		state.Services[svc.Name] = svc
	}
	return state, nil
}
