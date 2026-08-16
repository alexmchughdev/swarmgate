package observe

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"

	"github.com/alexmchughdev/swarmgate/internal/diff"
	"github.com/alexmchughdev/swarmgate/internal/spec"
)

func errNotFound(id string) error {
	return fmt.Errorf("no such service %s: %w", id, cerrdefs.ErrNotFound)
}

// pollStep is the cluster state one poll observes. AwaitConverged inspects
// each service exactly once per poll, so with a single watched service the
// script advances one step per poll; TaskList serves the step of the most
// recent inspect.
type pollStep struct {
	svc        swarm.Service
	inspectErr error
	tasks      []swarm.Task
}

type scriptedAPI struct {
	mu       sync.Mutex
	networks []network.Summary
	steps    []pollStep
	inspects int
}

func (f *scriptedAPI) stepAt(i int) pollStep {
	if i < 0 {
		i = 0
	}
	if i >= len(f.steps) {
		i = len(f.steps) - 1
	}
	return f.steps[i]
}

func (f *scriptedAPI) ServiceList(_ context.Context, _ swarm.ServiceListOptions) ([]swarm.Service, error) {
	return nil, nil
}

func (f *scriptedAPI) NetworkList(_ context.Context, _ network.ListOptions) ([]network.Summary, error) {
	return f.networks, nil
}

func (f *scriptedAPI) ServiceInspectWithRaw(_ context.Context, _ string, _ swarm.ServiceInspectOptions) (swarm.Service, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.stepAt(f.inspects)
	f.inspects++
	if s.inspectErr != nil {
		return swarm.Service{}, nil, s.inspectErr
	}
	return s.svc, nil, nil
}

func (f *scriptedAPI) TaskList(_ context.Context, _ swarm.TaskListOptions) ([]swarm.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stepAt(f.inspects - 1).tasks, nil
}

// Events is unused by this file's tests; see events_test.go for the
// dedicated fake that drives event-stream scenarios.
func (f *scriptedAPI) Events(_ context.Context, _ events.ListOptions) (<-chan events.Message, <-chan error) {
	return nil, nil
}

func desiredNginx() spec.ServiceSpec {
	return spec.Normalize("web", spec.ServiceSpec{
		Name:     "web_nginx",
		Image:    "nginx@sha256:abc",
		Replicas: 2,
	})
}

// liveNginx is the engine-side shape of desiredNginx: same normal form once
// mapped through FromSwarm with net-1 resolving to web_default.
func liveNginx(env []string, update *swarm.UpdateStatus) swarm.Service {
	replicas := uint64(2)
	return swarm.Service{
		Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{
				Name: "web_nginx",
				Labels: map[string]string{
					spec.ManagedLabel: "true",
					spec.StackLabel:   "web",
				},
			},
			TaskTemplate: swarm.TaskSpec{
				ContainerSpec: &swarm.ContainerSpec{Image: "nginx@sha256:abc", Env: env},
				Networks:      []swarm.NetworkAttachmentConfig{{Target: "net-1"}},
			},
			Mode: swarm.ServiceMode{
				Replicated: &swarm.ReplicatedService{Replicas: &replicas},
			},
		},
		UpdateStatus: update,
	}
}

func runningTask(image string) swarm.Task {
	return swarm.Task{
		DesiredState: swarm.TaskStateRunning,
		Status:       swarm.TaskStatus{State: swarm.TaskStateRunning},
		Spec:         swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: image}},
	}
}

func testNetworks() []network.Summary {
	return []network.Summary{{ID: "net-1", Name: "web_default"}}
}

func newTestObserver(api swarmAPI) *SwarmObserver {
	return &SwarmObserver{api: api, pollInterval: time.Millisecond}
}

func TestAwaitConvergedEmptyDiff(t *testing.T) {
	o := &SwarmObserver{}
	if err := o.AwaitConverged(context.Background(), diff.Diff{}, time.Second); err != nil {
		t.Fatalf("AwaitConverged(empty) = %v, want nil", err)
	}
}

func TestAwaitConvergedProgression(t *testing.T) {
	oneTask := []swarm.Task{runningTask("nginx@sha256:abc")}
	twoTasks := []swarm.Task{runningTask("nginx@sha256:abc"), runningTask("nginx@sha256:abc")}
	fake := &scriptedAPI{
		networks: testNetworks(),
		steps: []pollStep{
			{svc: liveNginx(nil, &swarm.UpdateStatus{State: swarm.UpdateStateUpdating}), tasks: oneTask},
			{svc: liveNginx(nil, &swarm.UpdateStatus{State: swarm.UpdateStateCompleted}), tasks: oneTask},
			{svc: liveNginx(nil, &swarm.UpdateStatus{State: swarm.UpdateStateCompleted}), tasks: twoTasks},
		},
	}
	o := newTestObserver(fake)

	d := diff.Diff{Updates: []diff.Change{{Name: "web_nginx", New: desiredNginx()}}}
	if err := o.AwaitConverged(context.Background(), d, time.Second); err != nil {
		t.Fatalf("AwaitConverged() = %v, want nil", err)
	}
	if fake.inspects != 3 {
		t.Errorf("inspect calls = %d, want 3 (one per poll)", fake.inspects)
	}
}

func TestAwaitConvergedTimeoutNamesServiceAndClause(t *testing.T) {
	fake := &scriptedAPI{
		networks: testNetworks(),
		steps: []pollStep{
			{svc: liveNginx(nil, nil), tasks: []swarm.Task{runningTask("nginx@sha256:abc")}},
		},
	}
	o := newTestObserver(fake)

	d := diff.Diff{Creates: []spec.ServiceSpec{desiredNginx()}}
	err := o.AwaitConverged(context.Background(), d, 25*time.Millisecond)
	if err == nil {
		t.Fatal("AwaitConverged() = nil, want timeout error")
	}
	want := "web_nginx: tasks running 1/2 (clause c)"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Errorf("error %q does not wrap the deadline", err)
	}
}

func TestAwaitConvergedRemove(t *testing.T) {
	fake := &scriptedAPI{
		steps: []pollStep{
			{svc: liveNginx(nil, nil)},
			{inspectErr: errNotFound("web_nginx")},
		},
	}
	o := newTestObserver(fake)

	d := diff.Diff{Removes: []spec.ServiceSpec{{Name: "web_nginx"}}}
	if err := o.AwaitConverged(context.Background(), d, time.Second); err != nil {
		t.Fatalf("AwaitConverged() = %v, want nil", err)
	}
	if fake.inspects != 2 {
		t.Errorf("inspect calls = %d, want 2 (present, then gone)", fake.inspects)
	}
}

func TestAwaitConvergedRemoveStillPresent(t *testing.T) {
	fake := &scriptedAPI{
		steps: []pollStep{{svc: liveNginx(nil, nil)}},
	}
	o := newTestObserver(fake)

	d := diff.Diff{Removes: []spec.ServiceSpec{{Name: "web_nginx"}}}
	err := o.AwaitConverged(context.Background(), d, 25*time.Millisecond)
	if err == nil {
		t.Fatal("AwaitConverged() = nil, want timeout error")
	}
	if want := "web_nginx: still present (remove)"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
}

func TestAwaitConvergedTaskStuckStartingNeverConverges(t *testing.T) {
	// A task whose healthcheck never passes stays in "starting" forever: the
	// engine holds it there rather than promoting it to "running". Clause
	// (c) counts only running tasks, so this must never be mistaken for
	// convergence, however long the wait.
	stuck := swarm.Task{
		DesiredState: swarm.TaskStateRunning,
		Status:       swarm.TaskStatus{State: swarm.TaskStateStarting},
		Spec:         swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "nginx@sha256:abc"}},
	}
	fake := &scriptedAPI{
		networks: testNetworks(),
		steps: []pollStep{
			{svc: liveNginx(nil, nil), tasks: []swarm.Task{stuck, stuck}},
		},
	}
	o := newTestObserver(fake)

	d := diff.Diff{Creates: []spec.ServiceSpec{desiredNginx()}}
	start := time.Now()
	err := o.AwaitConverged(context.Background(), d, 50*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("AwaitConverged() = nil, want timeout error: a task stuck in starting must never be reported as converged")
	}
	if elapsed < 50*time.Millisecond {
		t.Fatalf("AwaitConverged returned early after %s, want it to hold the full timeout while the task is stuck", elapsed)
	}
	if want := "web_nginx: tasks running 0/2 (clause c)"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Errorf("error %q does not wrap the deadline", err)
	}
}

func TestAwaitConvergedSpecMismatch(t *testing.T) {
	// Tasks satisfy clause (c); only the observed env differs from desired.
	fake := &scriptedAPI{
		networks: testNetworks(),
		steps: []pollStep{
			{
				svc:   liveNginx([]string{"MODE=debug"}, nil),
				tasks: []swarm.Task{runningTask("nginx@sha256:abc"), runningTask("nginx@sha256:abc")},
			},
		},
	}
	o := newTestObserver(fake)

	d := diff.Diff{Creates: []spec.ServiceSpec{desiredNginx()}}
	err := o.AwaitConverged(context.Background(), d, 25*time.Millisecond)
	if err == nil {
		t.Fatal("AwaitConverged() = nil, want timeout error")
	}
	if want := "web_nginx: observed spec differs from desired (clause a)"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
}
