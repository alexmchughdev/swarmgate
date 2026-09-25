package spec

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
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

		// Command/Entrypoint follow the Moby convention: ContainerSpec.Command
		// is the entrypoint override, ContainerSpec.Args is the cmd override.
		out.Entrypoint = cs.Command
		out.Command = cs.Args
		out.Hostname = cs.Hostname
		out.User = cs.User
		out.CapAdd = cs.CapabilityAdd
		if cs.StopGracePeriod != nil {
			out.StopGracePeriod = *cs.StopGracePeriod
		}
		for _, u := range cs.Ulimits {
			out.Ulimits = append(out.Ulimits, UlimitSpec{Name: u.Name, Soft: u.Soft, Hard: u.Hard})
		}
		for _, m := range cs.Mounts {
			if m.Type != mount.TypeVolume && m.Type != mount.TypeBind {
				continue
			}
			out.Volumes = append(out.Volumes, VolumeMount{Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
		}
		for _, c := range cs.Configs {
			target := ""
			if c.File != nil {
				target = c.File.Name
			}
			out.Configs = append(out.Configs, FileRef{Source: c.ConfigName, Target: target})
		}
		for _, sec := range cs.Secrets {
			target := ""
			if sec.File != nil {
				target = sec.File.Name
			}
			out.Secrets = append(out.Secrets, FileRef{Source: sec.SecretName, Target: target})
		}
	}

	switch {
	case s.Spec.Mode.Replicated != nil:
		out.Mode = "replicated"
	case s.Spec.Mode.Global != nil:
		out.Mode = "global"
	case s.Spec.Mode.ReplicatedJob != nil:
		out.Mode = "replicated-job"
	case s.Spec.Mode.GlobalJob != nil:
		out.Mode = "global-job"
	}

	if rp := s.Spec.TaskTemplate.RestartPolicy; rp != nil {
		out.RestartPolicy = &RestartPolicySpec{Condition: string(rp.Condition)}
		if rp.Delay != nil {
			out.RestartPolicy.Delay = *rp.Delay
		}
		if rp.MaxAttempts != nil {
			out.RestartPolicy.MaxAttempts = *rp.MaxAttempts
		}
		if rp.Window != nil {
			out.RestartPolicy.Window = *rp.Window
		}
	}

	if res := s.Spec.TaskTemplate.Resources; res != nil && res.Limits != nil {
		out.Resources = &ResourcesSpec{MemoryBytes: res.Limits.MemoryBytes, NanoCPUs: res.Limits.NanoCPUs}
	}

	if pl := s.Spec.TaskTemplate.Placement; pl != nil && len(pl.Constraints) > 0 {
		out.Placement = &PlacementSpec{Constraints: pl.Constraints}
	}

	if uc := s.Spec.UpdateConfig; uc != nil {
		out.UpdateConfig = &UpdateConfigSpec{Parallelism: uc.Parallelism}
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
		for _, p := range s.Spec.EndpointSpec.Ports {
			out.Ports = append(out.Ports, PortSpec{
				Target:    p.TargetPort,
				Published: p.PublishedPort,
				Protocol:  string(p.Protocol),
				Mode:      string(p.PublishMode),
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

	// Command/Entrypoint follow the Moby convention: ContainerSpec.Command
	// is the entrypoint override, ContainerSpec.Args is the cmd override —
	// the mirror of FromSwarm's own mapping above.
	cs.Command = s.Entrypoint
	cs.Args = s.Command
	cs.Hostname = s.Hostname
	cs.User = s.User
	cs.CapabilityAdd = s.CapAdd
	if s.StopGracePeriod > 0 {
		sgp := s.StopGracePeriod
		cs.StopGracePeriod = &sgp
	}
	for _, u := range s.Ulimits {
		cs.Ulimits = append(cs.Ulimits, &container.Ulimit{Name: u.Name, Soft: u.Soft, Hard: u.Hard})
	}
	for _, v := range s.Volumes {
		mountType := mount.TypeVolume
		if filepath.IsAbs(v.Source) {
			mountType = mount.TypeBind
		}
		cs.Mounts = append(cs.Mounts, mount.Mount{
			Type: mountType, Source: v.Source, Target: v.Target, ReadOnly: v.ReadOnly,
		})
	}
	// Configs/Secrets carry only the name here; ConfigID/SecretID are
	// resolved and attached by the applier, which is the only side with a
	// live connection to look external objects up by name (see
	// apply.ensureConfigsAndSecrets).
	for _, c := range s.Configs {
		cs.Configs = append(cs.Configs, &swarm.ConfigReference{
			ConfigName: c.Source,
			File:       &swarm.ConfigReferenceFileTarget{Name: c.Target},
		})
	}
	for _, sec := range s.Secrets {
		cs.Secrets = append(cs.Secrets, &swarm.SecretReference{
			SecretName: sec.Source,
			File:       &swarm.SecretReferenceFileTarget{Name: sec.Target},
		})
	}
	out.TaskTemplate.ContainerSpec = cs

	switch s.Mode {
	case "global":
		out.Mode = swarm.ServiceMode{Global: &swarm.GlobalService{}}
	case "replicated-job":
		out.Mode = swarm.ServiceMode{ReplicatedJob: &swarm.ReplicatedJob{}}
	case "global-job":
		out.Mode = swarm.ServiceMode{GlobalJob: &swarm.GlobalJob{}}
	default:
		// Copy before taking the address so the swarm spec never aliases
		// the caller's field.
		replicas := s.Replicas
		out.Mode = swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}}
	}

	if rp := s.RestartPolicy; rp != nil {
		out.TaskTemplate.RestartPolicy = &swarm.RestartPolicy{
			Condition: swarm.RestartPolicyCondition(rp.Condition),
		}
		if rp.Delay > 0 {
			out.TaskTemplate.RestartPolicy.Delay = &rp.Delay
		}
		if rp.MaxAttempts > 0 {
			ma := rp.MaxAttempts
			out.TaskTemplate.RestartPolicy.MaxAttempts = &ma
		}
		if rp.Window > 0 {
			out.TaskTemplate.RestartPolicy.Window = &rp.Window
		}
	}

	if res := s.Resources; res != nil {
		out.TaskTemplate.Resources = &swarm.ResourceRequirements{
			Limits: &swarm.Limit{MemoryBytes: res.MemoryBytes, NanoCPUs: res.NanoCPUs},
		}
	}

	if pl := s.Placement; pl != nil {
		out.TaskTemplate.Placement = &swarm.Placement{Constraints: pl.Constraints}
	}

	if uc := s.UpdateConfig; uc != nil {
		out.UpdateConfig = &swarm.UpdateConfig{Parallelism: uc.Parallelism}
	}

	// Normal form is sorted, so slice order here is already deterministic.
	for _, n := range s.Networks {
		out.TaskTemplate.Networks = append(out.TaskTemplate.Networks,
			swarm.NetworkAttachmentConfig{Target: n})
	}

	if len(s.Ports) > 0 {
		ep := &swarm.EndpointSpec{}
		for _, p := range s.Ports {
			ep.Ports = append(ep.Ports, swarm.PortConfig{
				Protocol:      swarm.PortConfigProtocol(p.Protocol),
				TargetPort:    p.Target,
				PublishedPort: p.Published,
				PublishMode:   swarm.PortConfigPublishMode(p.Mode),
			})
		}
		out.EndpointSpec = ep
	}

	return out
}
