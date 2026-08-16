package apply

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"

	"github.com/alexmchughdev/swarmgate/internal/spec"
)

// fakeServiceAPI records the last call per method and returns configured
// values, standing in for a Swarm manager.
type fakeServiceAPI struct {
	createSpec *swarm.ServiceSpec
	createErr  error

	inspectID  string
	inspectSvc swarm.Service
	inspectErr error

	updateID   string
	updateVer  swarm.Version
	updateSpec *swarm.ServiceSpec
	updateErr  error

	removedID string
	removeErr error

	existingNetworks []string
	createdNetworks  []network.CreateOptions
	createdNetNames  []string
	netListErr       error
	netCreateErr     error
	netInspectErr    error
	inspectedNetIDs  []string
}

func (f *fakeServiceAPI) NetworkList(_ context.Context, _ network.ListOptions) ([]network.Summary, error) {
	var out []network.Summary
	for _, n := range f.existingNetworks {
		out = append(out, network.Summary{Name: n})
	}
	return out, f.netListErr
}

func (f *fakeServiceAPI) NetworkCreate(_ context.Context, name string, options network.CreateOptions) (network.CreateResponse, error) {
	f.createdNetNames = append(f.createdNetNames, name)
	f.createdNetworks = append(f.createdNetworks, options)
	return network.CreateResponse{ID: "net-" + name}, f.netCreateErr
}

func (f *fakeServiceAPI) NetworkInspect(_ context.Context, networkID string, _ network.InspectOptions) (network.Inspect, error) {
	f.inspectedNetIDs = append(f.inspectedNetIDs, networkID)
	return network.Inspect{}, f.netInspectErr
}

func (f *fakeServiceAPI) ServiceCreate(_ context.Context, service swarm.ServiceSpec, _ swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error) {
	f.createSpec = &service
	return swarm.ServiceCreateResponse{ID: "created-id"}, f.createErr
}

func (f *fakeServiceAPI) ServiceUpdate(_ context.Context, serviceID string, version swarm.Version, service swarm.ServiceSpec, _ swarm.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error) {
	f.updateID = serviceID
	f.updateVer = version
	f.updateSpec = &service
	return swarm.ServiceUpdateResponse{}, f.updateErr
}

func (f *fakeServiceAPI) ServiceInspectWithRaw(_ context.Context, serviceID string, _ swarm.ServiceInspectOptions) (swarm.Service, []byte, error) {
	f.inspectID = serviceID
	return f.inspectSvc, nil, f.inspectErr
}

func (f *fakeServiceAPI) ServiceRemove(_ context.Context, serviceID string) error {
	f.removedID = serviceID
	return f.removeErr
}

func managedSpec() spec.ServiceSpec {
	return spec.Normalize("web", spec.ServiceSpec{
		Name:     "web_app",
		Image:    "registry.example.com/app@sha256:abc123",
		Replicas: 3,
		Env:      map[string]string{"FOO": "bar"},
		Networks: []string{"web_default"},
		Ports:    []spec.PortSpec{{Target: 8080, Published: 80, Protocol: "tcp"}},
	})
}

func TestCreatePassesThroughToSwarm(t *testing.T) {
	api := &fakeServiceAPI{}
	a := &SwarmApplier{api: api}
	s := managedSpec()

	if err := a.Apply(context.Background(), Change{Action: ActionCreate, Spec: s}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if api.createSpec == nil {
		t.Fatal("ServiceCreate was not called")
	}
	want := spec.ToSwarm(s)
	// The applier attaches networks by ID (the fake mints net-<name>).
	for i := range want.TaskTemplate.Networks {
		want.TaskTemplate.Networks[i].Target = "net-" + want.TaskTemplate.Networks[i].Target
	}
	if !reflect.DeepEqual(*api.createSpec, want) {
		t.Errorf("ServiceCreate spec = %+v, want %+v", *api.createSpec, want)
	}
	if api.createSpec.Annotations.Labels[spec.ManagedLabel] != "true" {
		t.Errorf("created spec is missing %s=true", spec.ManagedLabel)
	}
}

func TestCreateRefusesWithoutManagedLabel(t *testing.T) {
	api := &fakeServiceAPI{}
	a := &SwarmApplier{api: api}
	s := managedSpec()
	s.Labels = map[string]string{spec.StackLabel: "web"}

	err := a.Apply(context.Background(), Change{Action: ActionCreate, Spec: s})
	if err == nil {
		t.Fatal("Apply() expected error, got nil")
	}
	if !strings.Contains(err.Error(), spec.ManagedLabel) {
		t.Errorf("error %q does not mention %s", err, spec.ManagedLabel)
	}
	if api.createSpec != nil {
		t.Error("ServiceCreate was called despite the missing managed label")
	}
}

func TestUpdatePreservesUnmodelledFields(t *testing.T) {
	grace := 42 * time.Second
	api := &fakeServiceAPI{
		inspectSvc: swarm.Service{
			ID:   "svc-id",
			Meta: swarm.Meta{Version: swarm.Version{Index: 7}},
			Spec: swarm.ServiceSpec{
				Annotations: swarm.Annotations{
					Name:   "web_app",
					Labels: map[string]string{spec.ManagedLabel: "true", spec.StackLabel: "web"},
				},
				TaskTemplate: swarm.TaskSpec{
					ContainerSpec: &swarm.ContainerSpec{
						Image:           "registry.example.com/app@sha256:old",
						StopGracePeriod: &grace,
					},
					Placement: &swarm.Placement{Constraints: []string{"node.role==worker"}},
				},
				EndpointSpec: &swarm.EndpointSpec{Mode: swarm.ResolutionModeDNSRR},
			},
		},
	}
	a := &SwarmApplier{api: api}
	s := managedSpec()

	if err := a.Apply(context.Background(), Change{Action: ActionUpdate, Spec: s}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if api.inspectID != s.Name {
		t.Errorf("inspected %q, want %q", api.inspectID, s.Name)
	}
	if api.updateID != "svc-id" {
		t.Errorf("updated id %q, want %q", api.updateID, "svc-id")
	}
	if api.updateVer.Index != 7 {
		t.Errorf("update version index = %d, want 7", api.updateVer.Index)
	}
	if api.updateSpec == nil {
		t.Fatal("ServiceUpdate was not called")
	}
	got := *api.updateSpec

	// Modelled fields must match the desired spec.
	desired := spec.ToSwarm(s)
	if got.Annotations.Name != desired.Annotations.Name ||
		!reflect.DeepEqual(got.Annotations.Labels, desired.Annotations.Labels) {
		t.Errorf("annotations = %+v, want %+v", got.Annotations, desired.Annotations)
	}
	if got.TaskTemplate.ContainerSpec.Image != desired.TaskTemplate.ContainerSpec.Image {
		t.Errorf("image = %q, want %q", got.TaskTemplate.ContainerSpec.Image, desired.TaskTemplate.ContainerSpec.Image)
	}
	if !reflect.DeepEqual(got.TaskTemplate.ContainerSpec.Env, desired.TaskTemplate.ContainerSpec.Env) {
		t.Errorf("env = %v, want %v", got.TaskTemplate.ContainerSpec.Env, desired.TaskTemplate.ContainerSpec.Env)
	}
	if !reflect.DeepEqual(got.Mode, desired.Mode) {
		t.Errorf("mode = %+v, want %+v", got.Mode, desired.Mode)
	}
	wantNets := append([]swarm.NetworkAttachmentConfig(nil), desired.TaskTemplate.Networks...)
	for i := range wantNets {
		wantNets[i].Target = "net-" + wantNets[i].Target
	}
	if !reflect.DeepEqual(got.TaskTemplate.Networks, wantNets) {
		t.Errorf("networks = %+v, want %+v", got.TaskTemplate.Networks, wantNets)
	}
	if !reflect.DeepEqual(got.EndpointSpec.Ports, desired.EndpointSpec.Ports) {
		t.Errorf("ports = %+v, want %+v", got.EndpointSpec.Ports, desired.EndpointSpec.Ports)
	}

	// Unmodelled fields must survive.
	if got.TaskTemplate.ContainerSpec.StopGracePeriod == nil || *got.TaskTemplate.ContainerSpec.StopGracePeriod != grace {
		t.Error("StopGracePeriod was not carried over")
	}
	if !reflect.DeepEqual(got.TaskTemplate.Placement, &swarm.Placement{Constraints: []string{"node.role==worker"}}) {
		t.Errorf("placement = %+v, want the inspected constraint", got.TaskTemplate.Placement)
	}
	if got.EndpointSpec.Mode != swarm.ResolutionModeDNSRR {
		t.Errorf("endpoint mode = %q, want %q", got.EndpointSpec.Mode, swarm.ResolutionModeDNSRR)
	}
}

// TestUpdatePreservesOperatorSetLabels is a regression test: the update
// path previously replaced Annotations.Labels wholesale with the desired
// (Normalize-filtered, swarmgate.*-only) set, silently deleting any label
// an operator set directly that swarmgate's model doesn't cover — such as
// traefik.enable, which Traefik-on-Swarm reads from service labels. Only
// the swarmgate.* namespace may be replaced by an update; everything else
// on the live service must survive untouched.
func TestUpdatePreservesOperatorSetLabels(t *testing.T) {
	api := &fakeServiceAPI{
		inspectSvc: swarm.Service{
			ID:   "svc-id",
			Meta: swarm.Meta{Version: swarm.Version{Index: 7}},
			Spec: swarm.ServiceSpec{
				Annotations: swarm.Annotations{
					Name: "web_app",
					Labels: map[string]string{
						spec.ManagedLabel: "true",
						spec.StackLabel:   "web",
						"traefik.enable":  "true",
					},
				},
				TaskTemplate: swarm.TaskSpec{
					ContainerSpec: &swarm.ContainerSpec{Image: "registry.example.com/app@sha256:old"},
				},
			},
		},
	}
	a := &SwarmApplier{api: api}

	if err := a.Apply(context.Background(), Change{Action: ActionUpdate, Spec: managedSpec()}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if api.updateSpec == nil {
		t.Fatal("ServiceUpdate was not called")
	}
	got := api.updateSpec.Annotations.Labels
	if got["traefik.enable"] != "true" {
		t.Errorf("Labels = %+v, want traefik.enable=true preserved", got)
	}
	if got[spec.ManagedLabel] != "true" || got[spec.StackLabel] != "web" {
		t.Errorf("Labels = %+v, want the swarmgate.* namespace still set from desired", got)
	}
}

// TestMergeLabelsDropsStaleSwarmgateKey is a regression test for the
// removal side of the same fix: a swarmgate.*-prefixed label no longer
// present in desired (e.g. a compose label the operator deleted) must not
// linger, unlike a non-swarmgate label, which is never swarmgate's to
// remove.
func TestMergeLabelsDropsStaleSwarmgateKey(t *testing.T) {
	current := map[string]string{
		spec.ManagedLabel: "true",
		"swarmgate.tier":  "frontend",
		"traefik.enable":  "true",
	}
	desired := map[string]string{
		spec.ManagedLabel: "true",
		spec.StackLabel:   "web",
	}
	got := mergeLabels(current, desired)
	want := map[string]string{
		spec.ManagedLabel: "true",
		spec.StackLabel:   "web",
		"traefik.enable":  "true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeLabels = %+v, want %+v", got, want)
	}
}

func TestRemoveRefusesUnmanaged(t *testing.T) {
	api := &fakeServiceAPI{
		inspectSvc: swarm.Service{
			ID: "svc-id",
			Spec: swarm.ServiceSpec{
				Annotations: swarm.Annotations{
					Name:   "web_app",
					Labels: map[string]string{"com.docker.stack.namespace": "web"},
				},
			},
		},
	}
	a := &SwarmApplier{api: api}

	err := a.Apply(context.Background(), Change{Action: ActionRemove, Spec: managedSpec()})
	if err == nil {
		t.Fatal("Apply() expected error, got nil")
	}
	if !strings.Contains(err.Error(), spec.ManagedLabel) {
		t.Errorf("error %q does not mention %s", err, spec.ManagedLabel)
	}
	if api.removedID != "" {
		t.Errorf("ServiceRemove was called with %q despite unmanaged service", api.removedID)
	}
}

func TestRemoveManaged(t *testing.T) {
	api := &fakeServiceAPI{
		inspectSvc: swarm.Service{
			ID: "svc-id",
			Spec: swarm.ServiceSpec{
				Annotations: swarm.Annotations{
					Name:   "web_app",
					Labels: map[string]string{spec.ManagedLabel: "true", spec.StackLabel: "web"},
				},
			},
		},
	}
	a := &SwarmApplier{api: api}

	if err := a.Apply(context.Background(), Change{Action: ActionRemove, Spec: managedSpec()}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if api.removedID != "svc-id" {
		t.Errorf("ServiceRemove called with %q, want %q", api.removedID, "svc-id")
	}
}

func TestClientErrorsAreWrapped(t *testing.T) {
	sentinel := errors.New("daemon says no")
	tests := []struct {
		name   string
		api    *fakeServiceAPI
		change Change
		want   string
	}{
		{
			name:   "create",
			api:    &fakeServiceAPI{createErr: sentinel},
			change: Change{Action: ActionCreate, Spec: managedSpec()},
			want:   `create "web_app"`,
		},
		{
			name:   "update inspect",
			api:    &fakeServiceAPI{inspectErr: sentinel},
			change: Change{Action: ActionUpdate, Spec: managedSpec()},
			want:   `update "web_app"`,
		},
		{
			name: "update",
			api: &fakeServiceAPI{
				inspectSvc: swarm.Service{ID: "svc-id"},
				updateErr:  sentinel,
			},
			change: Change{Action: ActionUpdate, Spec: managedSpec()},
			want:   `update "web_app"`,
		},
		{
			name:   "remove inspect",
			api:    &fakeServiceAPI{inspectErr: sentinel},
			change: Change{Action: ActionRemove, Spec: managedSpec()},
			want:   `remove "web_app"`,
		},
		{
			name: "remove",
			api: &fakeServiceAPI{
				inspectSvc: swarm.Service{
					ID: "svc-id",
					Spec: swarm.ServiceSpec{
						Annotations: swarm.Annotations{
							Labels: map[string]string{spec.ManagedLabel: "true"},
						},
					},
				},
				removeErr: sentinel,
			},
			change: Change{Action: ActionRemove, Spec: managedSpec()},
			want:   `remove "web_app"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &SwarmApplier{api: tt.api}
			err := a.Apply(context.Background(), tt.change)
			if !errors.Is(err, sentinel) {
				t.Fatalf("Apply() error = %v, want wrapped sentinel", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestApplyUnknownAction(t *testing.T) {
	a := &SwarmApplier{api: &fakeServiceAPI{}}
	err := a.Apply(context.Background(), Change{Action: Action("scale"), Spec: managedSpec()})
	if err == nil {
		t.Fatal("Apply() expected error, got nil")
	}
}

func TestFakeApplierRecordsInOrder(t *testing.T) {
	f := &FakeApplier{}
	a := managedSpec()
	b := managedSpec()
	b.Name = "web_db"

	f.Fail("web_db", errors.New("boom"))
	if err := f.Apply(context.Background(), Change{Action: ActionCreate, Spec: a}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if err := f.Apply(context.Background(), Change{Action: ActionRemove, Spec: b}); err == nil {
		t.Fatal("Apply() expected injected error, got nil")
	}

	got := f.Applied()
	if len(got) != 1 || got[0].Action != ActionCreate || got[0].Spec.Name != "web_app" {
		t.Errorf("Applied() = %+v, want single create for web_app", got)
	}
}

func TestCreateEnsuresMissingNetworks(t *testing.T) {
	api := &fakeServiceAPI{}
	a := &SwarmApplier{api: api}
	if err := a.Apply(context.Background(), Change{Action: ActionCreate, Spec: managedSpec()}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(api.createdNetNames) != 1 || api.createdNetNames[0] != "web_default" {
		t.Fatalf("created networks = %v, want [web_default]", api.createdNetNames)
	}
	opts := api.createdNetworks[0]
	if opts.Driver != "overlay" || opts.Labels[spec.ManagedLabel] != "true" {
		t.Fatalf("network options = %+v", opts)
	}
}

func TestCreateSkipsExistingNetworks(t *testing.T) {
	api := &fakeServiceAPI{existingNetworks: []string{"web_default"}}
	a := &SwarmApplier{api: api}
	if err := a.Apply(context.Background(), Change{Action: ActionCreate, Spec: managedSpec()}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(api.createdNetNames) != 0 {
		t.Fatalf("existing network recreated: %v", api.createdNetNames)
	}
}

func TestCreateSubstringNetworkNameIsNotAMatch(t *testing.T) {
	// The engine's name filter matches substrings; an existing
	// "web_default_extra" must not satisfy a need for "web_default".
	api := &fakeServiceAPI{existingNetworks: []string{"web_default_extra"}}
	a := &SwarmApplier{api: api}
	if err := a.Apply(context.Background(), Change{Action: ActionCreate, Spec: managedSpec()}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(api.createdNetNames) != 1 || api.createdNetNames[0] != "web_default" {
		t.Fatalf("created networks = %v, want [web_default]", api.createdNetNames)
	}
}

func TestCreateAwaitsNetworkVisibility(t *testing.T) {
	api := &fakeServiceAPI{}
	a := &SwarmApplier{api: api}
	if err := a.Apply(context.Background(), Change{Action: ActionCreate, Spec: managedSpec()}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(api.inspectedNetIDs) != 1 || api.inspectedNetIDs[0] != "net-web_default" {
		t.Fatalf("read-back barrier not exercised: %v", api.inspectedNetIDs)
	}
}
