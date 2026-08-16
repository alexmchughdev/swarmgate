package diff

import (
	"reflect"
	"testing"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/spec"
)

// base returns a fully populated normal-form spec; tests mutate one field
// at a time from this shape.
func base() spec.ServiceSpec {
	return spec.ServiceSpec{
		Name:     "web_nginx",
		Image:    "docker.io/library/nginx@sha256:aaaa",
		Replicas: 2,
		Env:      map[string]string{"MODE": "prod"},
		Labels:   map[string]string{spec.ManagedLabel: "true", spec.StackLabel: "web"},
		Networks: []string{"web_default"},
		Ports:    []spec.PortSpec{{Target: 80, Published: 8080, Protocol: "tcp"}},
		Healthcheck: &spec.HealthcheckSpec{
			Test:     []string{"CMD-SHELL", "curl -f http://localhost/"},
			Interval: 5 * time.Second,
			Retries:  3,
		},
	}
}

func desired(specs ...spec.ServiceSpec) spec.DesiredState {
	d := spec.DesiredState{Services: map[string]spec.ServiceSpec{}}
	for _, s := range specs {
		d.Services[s.Name] = s
	}
	return d
}

func observed(specs ...spec.ServiceSpec) spec.ObservedState {
	o := spec.ObservedState{Services: map[string]spec.ServiceSpec{}}
	for _, s := range specs {
		o.Services[s.Name] = s
	}
	return o
}

func TestComputeEqualStatesIsEmpty(t *testing.T) {
	d := Compute(desired(base()), observed(base()))
	if !d.Empty() {
		t.Fatalf("diff of equal states not empty: %+v", d)
	}
}

func TestComputeCreatesAndRemoves(t *testing.T) {
	extra := base()
	extra.Name = "web_stale"
	d := Compute(desired(base()), observed(extra))
	if len(d.Creates) != 1 || d.Creates[0].Name != "web_nginx" {
		t.Fatalf("Creates = %+v", d.Creates)
	}
	if len(d.Removes) != 1 || d.Removes[0].Name != "web_stale" {
		t.Fatalf("Removes = %+v", d.Removes)
	}
	if len(d.Updates) != 0 {
		t.Fatalf("Updates = %+v", d.Updates)
	}
}

func TestComputeSingleFieldChanges(t *testing.T) {
	tests := []struct {
		field  string
		mutate func(*spec.ServiceSpec)
	}{
		{"image", func(s *spec.ServiceSpec) { s.Image = "docker.io/library/nginx@sha256:bbbb" }},
		{"replicas", func(s *spec.ServiceSpec) { s.Replicas = 5 }},
		{"env", func(s *spec.ServiceSpec) { s.Env = map[string]string{"MODE": "dev"} }},
		{"labels", func(s *spec.ServiceSpec) {
			s.Labels = map[string]string{spec.ManagedLabel: "true", spec.StackLabel: "web", "swarmgate.tier": "edge"}
		}},
		{"networks", func(s *spec.ServiceSpec) { s.Networks = []string{"web_default", "web_front"} }},
		{"ports", func(s *spec.ServiceSpec) { s.Ports = []spec.PortSpec{{Target: 80, Published: 9090, Protocol: "tcp"}} }},
		{"healthcheck", func(s *spec.ServiceSpec) { s.Healthcheck = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			want := base()
			tt.mutate(&want)
			d := Compute(desired(want), observed(base()))
			if len(d.Updates) != 1 {
				t.Fatalf("Updates = %+v", d.Updates)
			}
			u := d.Updates[0]
			if !reflect.DeepEqual(u.Changed, []string{tt.field}) {
				t.Fatalf("Changed = %v, want [%s]", u.Changed, tt.field)
			}
			if !reflect.DeepEqual(u.New, want) || !reflect.DeepEqual(u.Old, base()) {
				t.Fatalf("Old/New specs not carried faithfully")
			}
		})
	}
}

func TestComputeMultiFieldChangeOrder(t *testing.T) {
	want := base()
	want.Image = "docker.io/library/nginx@sha256:cccc"
	want.Replicas = 9
	want.Ports = nil
	d := Compute(desired(want), observed(base()))
	if len(d.Updates) != 1 {
		t.Fatalf("Updates = %+v", d.Updates)
	}
	if got := d.Updates[0].Changed; !reflect.DeepEqual(got, []string{"image", "replicas", "ports"}) {
		t.Fatalf("Changed = %v, want fixed order [image replicas ports]", got)
	}
}

// TestComputeDetectsRemovedEnvKey is a regression test: Env is compared in
// full, not just desired's keys. The observed side is the submitted
// service spec, not runtime container env, so an extra key there reflects
// a real prior deployment, not image-injected noise, and deleting an env
// var from a stack file must produce a real diff.
func TestComputeDetectsRemovedEnvKey(t *testing.T) {
	have := base()
	have.Env = map[string]string{"MODE": "prod", "LEGACY_VAR": "set"}
	d := Compute(desired(base()), observed(have))
	if len(d.Updates) != 1 || !reflect.DeepEqual(d.Updates[0].Changed, []string{"env"}) {
		t.Fatalf("extra observed env key must diff: %+v", d)
	}
}

func TestComputeMissingDesiredEnvKeyDiffs(t *testing.T) {
	have := base()
	have.Env = map[string]string{}
	d := Compute(desired(base()), observed(have))
	if len(d.Updates) != 1 || !reflect.DeepEqual(d.Updates[0].Changed, []string{"env"}) {
		t.Fatalf("missing desired env key must diff: %+v", d)
	}
}

func TestComputeDeterministicOrdering(t *testing.T) {
	a, b, c := base(), base(), base()
	a.Name, b.Name, c.Name = "web_a", "web_b", "web_c"
	x, y := base(), base()
	x.Name, y.Name = "web_x", "web_y"
	for i := 0; i < 20; i++ {
		d := Compute(desired(c, a, b), observed(y, x))
		if d.Creates[0].Name != "web_a" || d.Creates[1].Name != "web_b" || d.Creates[2].Name != "web_c" {
			t.Fatalf("Creates order not deterministic: %+v", d.Creates)
		}
		if d.Removes[0].Name != "web_x" || d.Removes[1].Name != "web_y" {
			t.Fatalf("Removes order not deterministic: %+v", d.Removes)
		}
	}
}
