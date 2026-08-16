package spec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"

	"github.com/alexmchughdev/swarmgate/internal/source"
)

// FR4 allowlists. Anything outside these sets is refused up front: a field
// swarmgate cannot apply must fail the parse rather than be dropped and
// silently not deployed.
var (
	allowedTopLevel = map[string]bool{
		"version":  true,
		"name":     true,
		"services": true,
		"networks": true,
	}
	allowedService = map[string]bool{
		"image":       true,
		"deploy":      true,
		"environment": true,
		"labels":      true,
		"networks":    true,
		"ports":       true,
		"healthcheck": true,
	}
	allowedDeploy = map[string]bool{
		"replicas": true,
	}
	allowedHealthcheck = map[string]bool{
		"test":         true,
		"interval":     true,
		"timeout":      true,
		"retries":      true,
		"start_period": true,
		"disable":      true,
	}
	allowedPort = map[string]bool{
		"target":    true,
		"published": true,
		"protocol":  true,
	}
)

// Resource limits bound how large a single service's desired state may be.
// These aren't compose-spec limits; anyone who can push a stack file can
// otherwise ask the Docker API to create an arbitrary number of replicas,
// port bindings, network attachments, or environment entries on the next
// reconcile cycle, exhausting the daemon or the host it runs on.
const (
	maxReplicas = 1000
	maxPorts    = 100
	maxNetworks = 50
	maxEnvVars  = 1000
)

// Parse turns stack files into the desired state in normal form. Each file
// is one compose document whose stack name is the file name; interpolation
// is resolved only against allowedEnvVars, an explicit allowlist of names
// read from swarmgate's own process environment (config's
// git.interpolation_vars) — never the full environment. Anyone who can
// push a stack file can read back whatever a referenced variable resolves
// to (deployed into a live service spec, or echoed into telemetry), so
// exposing the whole environment by default would let a stack file reach
// past its own trust boundary into host secrets that have nothing to do
// with it; a nil/empty allowlist means stack files see none of it.
func Parse(files []source.StackFile, allowedEnvVars []string) (DesiredState, error) {
	// The allowlist check runs over all files before any compose load so a
	// single run reports every unsupported field, not just the first.
	var errs []error
	for _, f := range files {
		errs = append(errs, checkAllowedFields(f.Name, f.Content)...)
	}
	if len(errs) > 0 {
		return DesiredState{}, errors.Join(errs...)
	}

	env := allowedEnv(allowedEnvVars)
	ds := DesiredState{Services: make(map[string]ServiceSpec)}
	// owner tracks which stack file first claimed each qualified service
	// name. ServiceName joins stack and service with an unescaped "_", so
	// two different stacks can produce the same qualified name (stack
	// "payments" service "db_writer" and stack "payments_db" service
	// "writer" both qualify to "payments_db_writer") — without this check
	// the second file parsed would silently overwrite the first's service
	// in the shared map, letting whoever controls one stack file redefine
	// a service they don't own.
	owner := make(map[string]string)
	for _, f := range files {
		if err := parseStack(f, env, ds.Services, owner); err != nil {
			return DesiredState{}, fmt.Errorf("stack %q: %w", f.Name, err)
		}
	}
	return ds, nil
}

// allowedEnv builds the interpolation environment from only the named
// variables, each looked up individually rather than snapshotting the
// full process environment. A name with no value set in the process
// environment is simply absent from the mapping, matching compose's own
// "undefined variable interpolates to empty" semantics.
func allowedEnv(names []string) types.Mapping {
	env := make(types.Mapping, len(names))
	for _, name := range names {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	return env
}

// checkAllowedFields walks the raw YAML for fields outside the FR4 surface.
// This runs on the raw document rather than the loaded project because
// compose-go decodes into typed structs, which makes "anything else"
// detection unreliable.
func checkAllowedFields(stack string, content []byte) []error {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return []error{fmt.Errorf("stack %q: %w", stack, err)}
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		// Shape problems are the compose loader's to report.
		return nil
	}

	var errs []error
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		switch {
		case !allowedTopLevel[key.Value]:
			errs = append(errs, fmt.Errorf("unsupported compose field %q in stack %q", key.Value, stack))
		case key.Value == "services" && value.Kind == yaml.MappingNode:
			errs = append(errs, checkServices(stack, value)...)
		}
	}
	return errs
}

func checkServices(stack string, services *yaml.Node) []error {
	var errs []error
	for i := 0; i+1 < len(services.Content); i += 2 {
		name, body := services.Content[i].Value, services.Content[i+1]
		if body.Kind != yaml.MappingNode {
			continue
		}
		qualified := ServiceName(stack, name)
		for j := 0; j+1 < len(body.Content); j += 2 {
			key, value := body.Content[j], body.Content[j+1]
			switch {
			case !allowedService[key.Value]:
				errs = append(errs, fmt.Errorf("unsupported compose field %q in service %q", key.Value, qualified))
			case key.Value == "deploy" && value.Kind == yaml.MappingNode:
				for k := 0; k+1 < len(value.Content); k += 2 {
					if sub := value.Content[k].Value; !allowedDeploy[sub] {
						errs = append(errs, fmt.Errorf("unsupported compose field %q in service %q", "deploy."+sub, qualified))
					}
				}
			case key.Value == "healthcheck" && value.Kind == yaml.MappingNode:
				for k := 0; k+1 < len(value.Content); k += 2 {
					if sub := value.Content[k].Value; !allowedHealthcheck[sub] {
						errs = append(errs, fmt.Errorf("unsupported compose field %q in service %q", "healthcheck."+sub, qualified))
					}
				}
			case key.Value == "ports" && value.Kind == yaml.SequenceNode:
				for _, item := range value.Content {
					if item.Kind != yaml.MappingNode {
						// Short syntax ("8080:80"): a scalar, no subkeys.
						continue
					}
					for k := 0; k+1 < len(item.Content); k += 2 {
						if sub := item.Content[k].Value; !allowedPort[sub] {
							errs = append(errs, fmt.Errorf("unsupported compose field %q in service %q", "ports."+sub, qualified))
						}
					}
				}
			}
		}
	}
	return errs
}

// parseStack loads one compose document and adds its services, in normal
// form, to out. owner records the first stack to claim each qualified
// name across the whole Parse call, so a collision with a different
// stack fails the parse instead of silently overwriting that service.
func parseStack(f source.StackFile, env types.Mapping, out map[string]ServiceSpec, owner map[string]string) error {
	details := types.ConfigDetails{
		ConfigFiles: []types.ConfigFile{{Filename: f.Name + ".yaml", Content: f.Content}},
		Environment: env,
	}
	project, err := loader.LoadWithContext(context.Background(), details, func(o *loader.Options) {
		o.SetProjectName(f.Name, true)
	})
	if err != nil {
		return err
	}

	for key, s := range project.Services {
		ports, err := servicePorts(s.Ports)
		if err != nil {
			return fmt.Errorf("service %q: %w", key, err)
		}
		healthcheck, err := serviceHealthcheck(s.HealthCheck)
		if err != nil {
			return fmt.Errorf("service %q: %w", key, err)
		}
		name := ServiceName(f.Name, key)
		if first, dup := owner[name]; dup && first != f.Name {
			return fmt.Errorf("service %q collides with a service of the same qualified name already defined by stack %q", name, first)
		}
		owner[name] = f.Name
		spec := Normalize(f.Name, ServiceSpec{
			Name:        name,
			Image:       s.Image,
			Replicas:    DefaultReplicas(deployReplicas(s.Deploy)),
			Env:         serviceEnv(s.Environment),
			Labels:      s.Labels,
			Networks:    serviceNetworks(project, s),
			Ports:       ports,
			Healthcheck: healthcheck,
		})
		if err := checkResourceLimits(name, spec); err != nil {
			return err
		}
		out[name] = spec
	}
	return nil
}

// checkResourceLimits rejects a service whose desired state exceeds one of
// the sanity limits above.
func checkResourceLimits(name string, s ServiceSpec) error {
	switch {
	case s.Replicas > maxReplicas:
		return fmt.Errorf("service %q: replicas %d exceeds the %d limit", name, s.Replicas, maxReplicas)
	case len(s.Ports) > maxPorts:
		return fmt.Errorf("service %q: %d ports exceeds the %d limit", name, len(s.Ports), maxPorts)
	case len(s.Networks) > maxNetworks:
		return fmt.Errorf("service %q: %d networks exceeds the %d limit", name, len(s.Networks), maxNetworks)
	case len(s.Env) > maxEnvVars:
		return fmt.Errorf("service %q: %d environment variables exceeds the %d limit", name, len(s.Env), maxEnvVars)
	default:
		return nil
	}
}

// deployReplicas preserves the omitted-vs-explicit-zero distinction that
// DefaultReplicas relies on.
func deployReplicas(d *types.DeployConfig) *uint64 {
	if d == nil || d.Replicas == nil {
		return nil
	}
	r := uint64(*d.Replicas)
	return &r
}

func serviceEnv(env types.MappingWithEquals) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		if v == nil {
			// Rule: a bare KEY entry with no ambient value becomes the empty
			// string; dropping it would silently change the deployed config.
			out[k] = ""
			continue
		}
		out[k] = *v
	}
	return out
}

// serviceNetworks maps service network keys to the project's resolved
// network names: compose-go names externals after their declaration and
// prefixes everything else with the project (stack) name.
func serviceNetworks(p *types.Project, s types.ServiceConfig) []string {
	nets := make([]string, 0, len(s.Networks))
	for key := range s.Networks {
		if n, ok := p.Networks[key]; ok && n.Name != "" {
			nets = append(nets, n.Name)
			continue
		}
		nets = append(nets, p.Name+"_"+key)
	}
	return nets
}

func servicePorts(in []types.ServicePortConfig) ([]PortSpec, error) {
	if len(in) == 0 {
		return nil, nil
	}
	ports := make([]PortSpec, 0, len(in))
	for _, p := range in {
		var published uint32
		if p.Published != "" {
			// compose-go keeps published as a string because the spec allows
			// ranges; those are outside FR4, so anything non-numeric fails.
			n, err := strconv.ParseUint(p.Published, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("published port %q: %w", p.Published, err)
			}
			published = uint32(n)
		}
		// Long-syntax mode is not part of the normal form; publication uses
		// the engine default (ingress).
		ports = append(ports, PortSpec{
			Target:    p.Target,
			Published: published,
			Protocol:  p.Protocol,
		})
	}
	return ports, nil
}

func serviceHealthcheck(hc *types.HealthCheckConfig) (*HealthcheckSpec, error) {
	// disable: true and test: ["NONE"] both mean "no healthcheck", the same
	// as omitting the section entirely.
	if hc == nil || hc.Disable || (len(hc.Test) > 0 && hc.Test[0] == "NONE") {
		return nil, nil
	}
	// A test-less healthcheck inherits the image's check. The observed
	// side reads that back as no healthcheck, causing a permanent phantom
	// diff. Rejected at parse, consistent with other unsupported fields.
	if len(hc.Test) == 0 {
		return nil, errors.New("healthcheck with no test is not supported: either define test or remove the healthcheck section (image-inherited healthchecks are unsupported)")
	}
	out := &HealthcheckSpec{Test: hc.Test}
	if hc.Interval != nil {
		out.Interval = time.Duration(*hc.Interval)
	}
	if hc.Timeout != nil {
		out.Timeout = time.Duration(*hc.Timeout)
	}
	if hc.StartPeriod != nil {
		out.StartPeriod = time.Duration(*hc.StartPeriod)
	}
	if hc.Retries != nil {
		out.Retries = int(*hc.Retries)
	}
	return out, nil
}
