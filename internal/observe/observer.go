package observe

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"

	"github.com/alexmchughdev/swarmgate/internal/spec"
)

// Observer produces the observed side of the desired/observed comparison.
type Observer interface {
	Snapshot(ctx context.Context) (spec.ObservedState, error)
}

// swarmAPI is the slice of the Docker client the observer actually uses,
// kept narrow so tests can substitute it without a live daemon.
type swarmAPI interface {
	ServiceList(ctx context.Context, options swarm.ServiceListOptions) ([]swarm.Service, error)
	NetworkList(ctx context.Context, options network.ListOptions) ([]network.Summary, error)
	ServiceInspectWithRaw(ctx context.Context, serviceID string, opts swarm.ServiceInspectOptions) (swarm.Service, []byte, error)
	TaskList(ctx context.Context, options swarm.TaskListOptions) ([]swarm.Task, error)
	Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error)
}

// SwarmObserver snapshots managed services from a live Swarm manager.
type SwarmObserver struct {
	api swarmAPI

	// pollInterval paces AwaitConverged; tests shrink it to keep the
	// convergence loop fast without a live daemon.
	pollInterval time.Duration

	// eventsBackoffMin/Max bound the delay between Events reconnect
	// attempts; tests shrink both to keep reconnect scenarios fast.
	eventsBackoffMin time.Duration
	eventsBackoffMax time.Duration

	// now is injected so DriftHint timestamps are deterministic in tests.
	now func() time.Time
}

// NewSwarmObserver connects to the Docker engine at host, or via the
// standard environment when host is empty.
func NewSwarmObserver(host string) (*SwarmObserver, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	c, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &SwarmObserver{
		api:              c,
		pollInterval:     time.Second,
		eventsBackoffMin: 500 * time.Millisecond,
		eventsBackoffMax: 30 * time.Second,
		now:              time.Now,
	}, nil
}
