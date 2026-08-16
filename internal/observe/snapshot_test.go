package observe

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"

	"github.com/alexmchughdev/swarmgate/internal/spec"
)

type fakeSwarmAPI struct {
	services       []swarm.Service
	networks       []network.Summary
	serviceListErr error
	networkListErr error

	gotServiceListOptions swarm.ServiceListOptions
}

func (f *fakeSwarmAPI) ServiceList(_ context.Context, options swarm.ServiceListOptions) ([]swarm.Service, error) {
	f.gotServiceListOptions = options
	if f.serviceListErr != nil {
		return nil, f.serviceListErr
	}
	return f.services, nil
}

func (f *fakeSwarmAPI) NetworkList(_ context.Context, options network.ListOptions) ([]network.Summary, error) {
	if f.networkListErr != nil {
		return nil, f.networkListErr
	}
	return f.networks, nil
}

func (f *fakeSwarmAPI) ServiceInspectWithRaw(_ context.Context, serviceID string, _ swarm.ServiceInspectOptions) (swarm.Service, []byte, error) {
	for _, s := range f.services {
		if s.Spec.Name == serviceID {
			return s, nil, nil
		}
	}
	return swarm.Service{}, nil, errNotFound(serviceID)
}

func (f *fakeSwarmAPI) TaskList(_ context.Context, _ swarm.TaskListOptions) ([]swarm.Task, error) {
	return nil, nil
}

// Events is unused by this file's tests; see events_test.go for the
// dedicated fake that drives event-stream scenarios.
func (f *fakeSwarmAPI) Events(_ context.Context, _ events.ListOptions) (<-chan events.Message, <-chan error) {
	return nil, nil
}

func managedService(name, stack, image string, replicas uint64, networkTarget string) swarm.Service {
	return swarm.Service{
		Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{
				Name: name,
				Labels: map[string]string{
					spec.ManagedLabel: "true",
					spec.StackLabel:   stack,
				},
			},
			TaskTemplate: swarm.TaskSpec{
				ContainerSpec: &swarm.ContainerSpec{Image: image},
				Networks: []swarm.NetworkAttachmentConfig{
					{Target: networkTarget},
				},
			},
			Mode: swarm.ServiceMode{
				Replicated: &swarm.ReplicatedService{Replicas: &replicas},
			},
		},
	}
}

func TestSwarmObserverSnapshot(t *testing.T) {
	fake := &fakeSwarmAPI{
		services: []swarm.Service{
			managedService("web_app", "web", "app@sha256:aaa", 2, "net-id-1"),
			managedService("web_db", "web", "db@sha256:bbb", 1, "net-id-1"),
		},
		networks: []network.Summary{
			{ID: "net-id-1", Name: "web_default"},
			{ID: "net-id-2", Name: "other"},
		},
	}
	o := &SwarmObserver{api: fake}

	state, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	if len(state.Services) != 2 {
		t.Fatalf("got %d services, want 2", len(state.Services))
	}
	for _, name := range []string{"web_app", "web_db"} {
		svc, ok := state.Services[name]
		if !ok {
			t.Fatalf("missing service %q in snapshot", name)
		}
		if svc.Name != name {
			t.Errorf("service keyed %q has Name %q", name, svc.Name)
		}
	}
	if got := state.Services["web_app"].Networks; len(got) != 1 || got[0] != "web_default" {
		t.Errorf("web_app networks = %v, want [web_default]", got)
	}
	if got := state.Services["web_app"].Replicas; got != 2 {
		t.Errorf("web_app replicas = %d, want 2", got)
	}

	labels := fake.gotServiceListOptions.Filters.Get("label")
	if len(labels) != 1 || labels[0] != spec.ManagedLabel+"=true" {
		t.Errorf("ServiceList label filter = %v, want [%s=true]", labels, spec.ManagedLabel)
	}
}

func TestSwarmObserverSnapshotServiceListError(t *testing.T) {
	sentinel := errors.New("daemon unavailable")
	o := &SwarmObserver{api: &fakeSwarmAPI{serviceListErr: sentinel}}

	_, err := o.Snapshot(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("Snapshot() error = %v, want wrapped %v", err, sentinel)
	}
}

func TestSwarmObserverSnapshotNetworkListError(t *testing.T) {
	sentinel := errors.New("daemon unavailable")
	o := &SwarmObserver{api: &fakeSwarmAPI{networkListErr: sentinel}}

	_, err := o.Snapshot(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("Snapshot() error = %v, want wrapped %v", err, sentinel)
	}
}
