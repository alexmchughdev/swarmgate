package spec

import (
	"reflect"
	"testing"
	"time"
)

func TestServiceName(t *testing.T) {
	if got, want := ServiceName("web", "nginx"), "web_nginx"; got != want {
		t.Fatalf("ServiceName = %q, want %q", got, want)
	}
}

func TestDefaultNetwork(t *testing.T) {
	if got, want := DefaultNetwork("web"), "web_default"; got != want {
		t.Fatalf("DefaultNetwork = %q, want %q", got, want)
	}
}

func TestDefaultReplicas(t *testing.T) {
	zero, three := uint64(0), uint64(3)
	tests := []struct {
		name string
		in   *uint64
		want uint64
	}{
		{"omitted defaults to one", nil, 1},
		{"explicit zero preserved", &zero, 0},
		{"explicit value preserved", &three, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DefaultReplicas(tt.in); got != tt.want {
				t.Fatalf("DefaultReplicas = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   ServiceSpec
		want ServiceSpec
	}{
		{
			name: "labels filtered to swarmgate prefix and managed always set",
			in: ServiceSpec{Labels: map[string]string{
				"swarmgate.tier":              "backend",
				"com.docker.stack.namespace":  "web",
				"traefik.enable":              "true",
				"com.example.operator.notice": "x",
			}},
			want: ServiceSpec{
				Labels:   map[string]string{"swarmgate.tier": "backend", ManagedLabel: "true", StackLabel: "web"},
				Networks: []string{"web_default"},
				Mode:     "replicated",
			},
		},
		{
			name: "managed label set when no labels given",
			in:   ServiceSpec{},
			want: ServiceSpec{
				Labels:   map[string]string{ManagedLabel: "true", StackLabel: "web"},
				Networks: []string{"web_default"},
				Mode:     "replicated",
			},
		},
		{
			name: "empty networks get explicit stack default",
			in:   ServiceSpec{Networks: nil},
			want: ServiceSpec{
				Labels:   map[string]string{ManagedLabel: "true", StackLabel: "web"},
				Networks: []string{"web_default"},
				Mode:     "replicated",
			},
		},
		{
			name: "networks sorted and deduplicated",
			in:   ServiceSpec{Networks: []string{"zeta", "alpha", "zeta", "mid"}},
			want: ServiceSpec{
				Labels:   map[string]string{ManagedLabel: "true", StackLabel: "web"},
				Networks: []string{"alpha", "mid", "zeta"},
				Mode:     "replicated",
			},
		},
		{
			name: "ports defaulted to tcp, lowercased, sorted",
			in: ServiceSpec{Ports: []PortSpec{
				{Target: 9000, Published: 9000, Protocol: "UDP"},
				{Target: 8080, Published: 81, Protocol: ""},
				{Target: 8080, Published: 80, Protocol: "TCP"},
				{Target: 8080, Published: 80, Protocol: "sctp"},
			}},
			want: ServiceSpec{
				Labels:   map[string]string{ManagedLabel: "true", StackLabel: "web"},
				Networks: []string{"web_default"},
				Ports: []PortSpec{
					{Target: 8080, Published: 80, Protocol: "sctp", Mode: "ingress"},
					{Target: 8080, Published: 80, Protocol: "tcp", Mode: "ingress"},
					{Target: 8080, Published: 81, Protocol: "tcp", Mode: "ingress"},
					{Target: 9000, Published: 9000, Protocol: "udp", Mode: "ingress"},
				},
				Mode: "replicated",
			},
		},
		{
			name: "env and image pass through untouched",
			in: ServiceSpec{
				Image: "nginx:1.27",
				Env:   map[string]string{"B": "2", "A": "1", "EMPTY": ""},
			},
			want: ServiceSpec{
				Image:    "nginx:1.27",
				Env:      map[string]string{"A": "1", "B": "2", "EMPTY": ""},
				Labels:   map[string]string{ManagedLabel: "true", StackLabel: "web"},
				Networks: []string{"web_default"},
				Mode:     "replicated",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Normalize("web", tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Normalize mismatch\n got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

func TestNormalizeDoesNotMutateInput(t *testing.T) {
	in := ServiceSpec{
		Networks: []string{"b", "a"},
		Ports:    []PortSpec{{Target: 2, Published: 2, Protocol: "TCP"}, {Target: 1, Published: 1}},
	}
	Normalize("web", in)
	if !reflect.DeepEqual(in.Networks, []string{"b", "a"}) {
		t.Fatalf("input networks mutated: %v", in.Networks)
	}
	if in.Ports[0].Protocol != "TCP" || in.Ports[1].Protocol != "" {
		t.Fatalf("input ports mutated: %v", in.Ports)
	}
}

func TestNormalizeEmptyCollectionsAreNil(t *testing.T) {
	got := Normalize("web", ServiceSpec{
		Env:   map[string]string{},
		Ports: []PortSpec{},
	})
	if got.Env != nil {
		t.Fatalf("empty env must normalise to nil, got %#v", got.Env)
	}
	if got.Ports != nil {
		t.Fatalf("empty ports must normalise to nil, got %#v", got.Ports)
	}
}

// TestNormalizeTestlessHealthcheckIsNil is a regression test: a healthcheck
// with no test must normalise to nil, matching what FromSwarm reads back.
func TestNormalizeTestlessHealthcheckIsNil(t *testing.T) {
	got := Normalize("web", ServiceSpec{
		Healthcheck: &HealthcheckSpec{Interval: 30 * time.Second},
	})
	if got.Healthcheck != nil {
		t.Fatalf("test-less healthcheck must normalise to nil, got %#v", got.Healthcheck)
	}
}
