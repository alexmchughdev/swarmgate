package spec

import (
	"slices"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/swarm"
)

// FromSwarm maps an engine-side service into normal form. It is the observer
// half of the Swarm mapping; the applier's ToSwarm mirror lives alongside it
// so the two directions stay in one file and drift is visible in review.
//
// networkNames resolves network IDs to names; a target absent from the map is
// used verbatim, since it is then already a name.
func FromSwarm(s swarm.Service, networkNames map[string]string) ServiceSpec {
	var out ServiceSpec

	out.Name = s.Spec.Name

	// Replicas: managed services are always replicated, so non-replicated
	// modes collapse to 0 and will surface as a diff rather than a crash.
	if s.Spec.Mode.Replicated != nil && s.Spec.Mode.Replicated.Replicas != nil {
		out.Replicas = *s.Spec.Mode.Replicated.Replicas
	}

	if cs := s.Spec.TaskTemplate.ContainerSpec; cs != nil {
		// Image is taken as stored: the desired side is digest-pinned
		// before apply, so submitted references are repo@sha256 and the
		// engine stores them verbatim.
		out.Image = cs.Image

		if len(cs.Env) > 0 {
			out.Env = make(map[string]string, len(cs.Env))
			for _, kv := range cs.Env {
				k, v, _ := strings.Cut(kv, "=")
				out.Env[k] = v
			}
		}

		if hc := cs.Healthcheck; hc != nil && len(hc.Test) > 0 {
			out.Healthcheck = &HealthcheckSpec{
				Test:        hc.Test,
				Interval:    hc.Interval,
				Timeout:     hc.Timeout,
				Retries:     hc.Retries,
				StartPeriod: hc.StartPeriod,
			}
		}
	}

	// Service labels, not container labels: the managed-label filter and
	// the prune guard both operate on the service annotations.
	out.Labels = s.Spec.Annotations.Labels

	for _, n := range s.Spec.TaskTemplate.Networks {
		name, ok := networkNames[n.Target]
		if !ok {
			name = n.Target
		}
		out.Networks = append(out.Networks, name)
	}

	if s.Spec.EndpointSpec != nil {
		// PublishMode is intentionally dropped: FR4 does not model it.
		for _, p := range s.Spec.EndpointSpec.Ports {
			out.Ports = append(out.Ports, PortSpec{
				Target:    p.TargetPort,
				Published: p.PublishedPort,
				Protocol:  string(p.Protocol),
			})
		}
	}

	stack := s.Spec.Annotations.Labels[StackLabel]
	return Normalize(stack, out)
}

// ToSwarm maps a normal-form spec into the engine-side service spec. It is
// the applier half of the Swarm mapping and must stay the mirror of FromSwarm
// above: for any normal-form s, FromSwarm(ToSwarm(s)) == s.
func ToSwarm(s ServiceSpec) swarm.ServiceSpec {
	var out swarm.ServiceSpec

	// Labels are taken as-is: normal form already carries ManagedLabel and
	// StackLabel, so no re-stamping happens here.
	out.Annotations = swarm.Annotations{Name: s.Name, Labels: s.Labels}

	cs := &swarm.ContainerSpec{Image: s.Image}
	if len(s.Env) > 0 {
		env := make([]string, 0, len(s.Env))
		for k, v := range s.Env {
			env = append(env, k+"="+v)
		}
		// Sorted so the engine sees a deterministic spec and the
		// round-trip through FromSwarm is stable.
		slices.Sort(env)
		cs.Env = env
	}
	if hc := s.Healthcheck; hc != nil {
		cs.Healthcheck = &container.HealthConfig{
			Test:        hc.Test,
			Interval:    hc.Interval,
			Timeout:     hc.Timeout,
			Retries:     hc.Retries,
			StartPeriod: hc.StartPeriod,
		}
	}
	out.TaskTemplate.ContainerSpec = cs

	// Copy before taking the address so the swarm spec never aliases the
	// caller's field.
	replicas := s.Replicas
	out.Mode = swarm.ServiceMode{
		Replicated: &swarm.ReplicatedService{Replicas: &replicas},
	}

	// Normal form is sorted, so slice order here is already deterministic.
	for _, n := range s.Networks {
		out.TaskTemplate.Networks = append(out.TaskTemplate.Networks,
			swarm.NetworkAttachmentConfig{Target: n})
	}

	if len(s.Ports) > 0 {
		ep := &swarm.EndpointSpec{}
		for _, p := range s.Ports {
			// PublishMode is left at the engine default: FR4 does
			// not model it.
			ep.Ports = append(ep.Ports, swarm.PortConfig{
				Protocol:      swarm.PortConfigProtocol(p.Protocol),
				TargetPort:    p.Target,
				PublishedPort: p.Published,
			})
		}
		out.EndpointSpec = ep
	}

	return out
}
