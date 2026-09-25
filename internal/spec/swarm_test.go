package spec

import (
	"reflect"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/swarm"
)

func uint64ptr(v uint64) *uint64 { return &v }

func TestFromSwarm(t *testing.T) {
	networkNames := map[string]string{
		"net-id-1": "web_default",
		"net-id-2": "web_frontend",
	}

	tests := []struct {
		name     string
		service  swarm.Service
		networks map[string]string
		want     ServiceSpec
	}{
		{
			name: "full service",
			service: swarm.Service{
				Spec: swarm.ServiceSpec{
					Annotations: swarm.Annotations{
						Name: "web_app",
						Labels: map[string]string{
							ManagedLabel:                  "true",
							StackLabel:                    "web",
							"swarmgate.extra":             "kept",
							"com.docker.stack.namespace":  "web",
							"operator.example.com/notice": "dropped",
						},
					},
					TaskTemplate: swarm.TaskSpec{
						ContainerSpec: &swarm.ContainerSpec{
							Image: "registry.example.com/app@sha256:abc123",
							Env:   []string{"FOO=bar", "EMPTY=", "BARE"},
							Healthcheck: &container.HealthConfig{
								Test:        []string{"CMD", "curl", "-f", "http://localhost/"},
								Interval:    10 * time.Second,
								Timeout:     3 * time.Second,
								Retries:     5,
								StartPeriod: 30 * time.Second,
							},
						},
						Networks: []swarm.NetworkAttachmentConfig{
							{Target: "net-id-2"},
							{Target: "net-id-1"},
						},
					},
					Mode: swarm.ServiceMode{
						Replicated: &swarm.ReplicatedService{Replicas: uint64ptr(3)},
					},
					EndpointSpec: &swarm.EndpointSpec{
						Ports: []swarm.PortConfig{
							{TargetPort: 8080, PublishedPort: 80, Protocol: swarm.PortConfigProtocolTCP},
							{TargetPort: 53, PublishedPort: 53, Protocol: swarm.PortConfigProtocol("UDP")},
						},
					},
				},
			},
			networks: networkNames,
			want: ServiceSpec{
				Name:     "web_app",
				Image:    "registry.example.com/app@sha256:abc123",
				Replicas: 3,
				Env:      map[string]string{"FOO": "bar", "EMPTY": "", "BARE": ""},
				Labels: map[string]string{
					ManagedLabel:      "true",
					StackLabel:        "web",
					"swarmgate.extra": "kept",
				},
				DeployLabels: map[string]string{"operator.example.com/notice": "dropped"},
				Networks:     []string{"web_default", "web_frontend"},
				Ports: []PortSpec{
					{Target: 53, Published: 53, Protocol: "udp", Mode: "ingress"},
					{Target: 8080, Published: 80, Protocol: "tcp", Mode: "ingress"},
				},
				Healthcheck: &HealthcheckSpec{
					Test:        []string{"CMD", "curl", "-f", "http://localhost/"},
					Interval:    10 * time.Second,
					Timeout:     3 * time.Second,
					Retries:     5,
					StartPeriod: 30 * time.Second,
				},
				Mode: "replicated",
			},
		},
		{
			name: "replicated with nil replicas",
			service: swarm.Service{
				Spec: swarm.ServiceSpec{
					Annotations: swarm.Annotations{
						Name:   "web_worker",
						Labels: map[string]string{StackLabel: "web"},
					},
					TaskTemplate: swarm.TaskSpec{
						ContainerSpec: &swarm.ContainerSpec{Image: "img@sha256:def"},
					},
					Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{}},
				},
			},
			networks: networkNames,
			want: ServiceSpec{
				Name:     "web_worker",
				Image:    "img@sha256:def",
				Replicas: 0,
				Labels:   map[string]string{ManagedLabel: "true", StackLabel: "web"},
				Networks: []string{"web_default"},
				Mode:     "replicated",
			},
		},
		{
			name: "global mode maps to zero replicas",
			service: swarm.Service{
				Spec: swarm.ServiceSpec{
					Annotations: swarm.Annotations{
						Name:   "web_agent",
						Labels: map[string]string{StackLabel: "web"},
					},
					TaskTemplate: swarm.TaskSpec{
						ContainerSpec: &swarm.ContainerSpec{Image: "img@sha256:012"},
					},
					Mode: swarm.ServiceMode{Global: &swarm.GlobalService{}},
				},
			},
			networks: networkNames,
			want: ServiceSpec{
				Name:     "web_agent",
				Image:    "img@sha256:012",
				Replicas: 0,
				Labels:   map[string]string{ManagedLabel: "true", StackLabel: "web"},
				Networks: []string{"web_default"},
				Mode:     "global",
			},
		},
		{
			name: "unmapped network target passes through verbatim",
			service: swarm.Service{
				Spec: swarm.ServiceSpec{
					Annotations: swarm.Annotations{
						Name:   "web_db",
						Labels: map[string]string{StackLabel: "web"},
					},
					TaskTemplate: swarm.TaskSpec{
						ContainerSpec: &swarm.ContainerSpec{Image: "img@sha256:345"},
						Networks: []swarm.NetworkAttachmentConfig{
							{Target: "web_backend"},
							{Target: "net-id-1"},
						},
					},
					Mode: swarm.ServiceMode{
						Replicated: &swarm.ReplicatedService{Replicas: uint64ptr(1)},
					},
				},
			},
			networks: networkNames,
			want: ServiceSpec{
				Name:     "web_db",
				Image:    "img@sha256:345",
				Replicas: 1,
				Labels:   map[string]string{ManagedLabel: "true", StackLabel: "web"},
				Networks: []string{"web_backend", "web_default"},
				Mode:     "replicated",
			},
		},
		{
			name: "healthcheck with empty test maps to nil",
			service: swarm.Service{
				Spec: swarm.ServiceSpec{
					Annotations: swarm.Annotations{
						Name:   "web_cache",
						Labels: map[string]string{StackLabel: "web"},
					},
					TaskTemplate: swarm.TaskSpec{
						ContainerSpec: &swarm.ContainerSpec{
							Image:       "img@sha256:678",
							Healthcheck: &container.HealthConfig{Interval: time.Second},
						},
					},
					Mode: swarm.ServiceMode{
						Replicated: &swarm.ReplicatedService{Replicas: uint64ptr(2)},
					},
				},
			},
			networks: networkNames,
			want: ServiceSpec{
				Name:     "web_cache",
				Image:    "img@sha256:678",
				Replicas: 2,
				Labels:   map[string]string{ManagedLabel: "true", StackLabel: "web"},
				Networks: []string{"web_default"},
				Mode:     "replicated",
			},
		},
		{
			name: "no healthcheck",
			service: swarm.Service{
				Spec: swarm.ServiceSpec{
					Annotations: swarm.Annotations{
						Name:   "web_plain",
						Labels: map[string]string{StackLabel: "web"},
					},
					TaskTemplate: swarm.TaskSpec{
						ContainerSpec: &swarm.ContainerSpec{Image: "img@sha256:901"},
					},
					Mode: swarm.ServiceMode{
						Replicated: &swarm.ReplicatedService{Replicas: uint64ptr(1)},
					},
				},
			},
			networks: networkNames,
			want: ServiceSpec{
				Name:     "web_plain",
				Image:    "img@sha256:901",
				Replicas: 1,
				Labels:   map[string]string{ManagedLabel: "true", StackLabel: "web"},
				Networks: []string{"web_default"},
				Mode:     "replicated",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromSwarm(tt.service, tt.networks)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("FromSwarm() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestToSwarmRoundTrip pins the mirror property the applier relies on:
// FromSwarm(ToSwarm(s)) == s for any normal-form spec. Network names pass
// through verbatim (nil name map), matching what ToSwarm submits.
func TestToSwarmRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   ServiceSpec
	}{
		{
			name: "full",
			in: Normalize("web", ServiceSpec{
				Name:     "web_app",
				Image:    "registry.example.com/app@sha256:abc123",
				Replicas: 3,
				Env:      map[string]string{"FOO": "bar", "EMPTY": "", "ZED": "z"},
				Labels: map[string]string{
					ManagedLabel:      "true",
					StackLabel:        "web",
					"swarmgate.extra": "kept",
				},
				Networks: []string{"web_default", "web_frontend"},
				Volumes: []VolumeMount{
					{Source: "shared_data", Target: "/var/lib/data"},
					{Source: "/srv/share", Target: "/srv/share", ReadOnly: true},
				},
				Ports: []PortSpec{
					{Target: 53, Published: 53, Protocol: "udp"},
					{Target: 8080, Published: 80, Protocol: "tcp"},
				},
				Healthcheck: &HealthcheckSpec{
					Test:        []string{"CMD", "curl", "-f", "http://localhost/"},
					Interval:    10 * time.Second,
					Timeout:     3 * time.Second,
					Retries:     5,
					StartPeriod: 30 * time.Second,
				},
			}),
		},
		{
			name: "minimal",
			in: Normalize("web", ServiceSpec{
				Name:     "web_plain",
				Image:    "img@sha256:901",
				Replicas: 1,
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromSwarm(swarm.Service{Spec: ToSwarm(tt.in)}, nil)
			if !reflect.DeepEqual(got, tt.in) {
				t.Errorf("FromSwarm(ToSwarm()) = %+v, want %+v", got, tt.in)
			}
		})
	}
}

// TestToSwarmEnvSorted pins determinism: identical env maps must always
// produce the same engine-side slice.
func TestToSwarmEnvSorted(t *testing.T) {
	s := Normalize("web", ServiceSpec{
		Name:     "web_app",
		Image:    "img@sha256:abc",
		Replicas: 1,
		Env:      map[string]string{"ZED": "z", "FOO": "bar", "EMPTY": ""},
	})
	got := ToSwarm(s).TaskTemplate.ContainerSpec.Env
	want := []string{"EMPTY=", "FOO=bar", "ZED=z"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("env = %v, want %v", got, want)
	}
}
