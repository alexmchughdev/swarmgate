package spec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"

	"github.com/alexmchughdev/swarmgate/internal/source"
)

// Allowlists. Anything outside these sets is refused up front: a field
// swarmgate cannot apply must fail the parse rather than be dropped and
// silently not deployed. A top-level key prefixed "x-" is always allowed
// regardless of this list — it's a compose extension field, inert to both
// the loader and swarmgate, commonly used to define a YAML anchor
// (`x-restart-policy: &restart-policy ...`) referenced elsewhere via a
// `*restart-policy` alias. Anchors and aliases themselves are plain YAML,
// resolved by the parser before swarmgate ever sees a key or value — no
// special handling beyond permitting the x- key they're commonly attached
// to.
var (
	allowedTopLevel = map[string]bool{
		"version":  true,
		"name":     true,
		"services": true,
		"networks": true,
		"volumes":  true,
		"configs":  true,
		"secrets":  true,
	}
	allowedService = map[string]bool{
		"image":             true,
		"deploy":            true,
		"environment":       true,
		"labels":            true,
		"networks":          true,
		"ports":             true,
		"healthcheck":       true,
		"volumes":           true,
		"env_file":          true,
		"configs":           true,
		"secrets":           true,
		"command":           true,
		"entrypoint":        true,
		"hostname":          true,
		"cap_add":           true,
		"user":              true,
		"stop_grace_period": true,
		"ulimits":           true,
	}
	allowedDeploy = map[string]bool{
		"replicas":       true,
		"mode":           true,
		"restart_policy": true,
		"resources":      true,
		"placement":      true,
		"update_config":  true,
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
		"mode":      true,
	}
	// allowedFileRef is the service-level configs/secrets reference shape:
	// which object to mount and where. uid/gid/mode (file ownership and
	// permission overrides) are not modelled.
	allowedFileRef = map[string]bool{
		"source": true,
		"target": true,
	}
	// allowedExternalObject is the top-level configs/secrets declaration
	// shape: external only, never inline file/content/environment, so a
	// stack file can reference a cluster object but never define one or
	// smuggle secret material through git.
	allowedExternalObject = map[string]bool{
		"external": true,
	}
)

// deployModes are the values deploy.mode accepts.
var deployModes = map[string]bool{
	"replicated":     true,
	"global":         true,
	"replicated-job": true,
	"global-job":     true,
}

// Resource limits bound how large a single service's desired state may be.
// These aren't compose-spec limits; anyone who can push a stack file can
// otherwise ask the Docker API to create an arbitrary number of replicas,
// port bindings, network attachments, or environment entries on the next
// reconcile cycle, exhausting the daemon or the host it runs on.
const (
	maxReplicas             = 1000
	maxPorts                = 100
	maxNetworks             = 50
	maxEnvVars              = 1000
	maxVolumes              = 50
	maxConfigs              = 50
	maxSecrets              = 50
	maxCapAdd               = 50
	maxUlimits              = 20
	maxCommandArgs          = 200
	maxPlacementConstraints = 50
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

func Parse(files []source.StackFile, allowedEnvVars []string, envFileRoots ...string) (DesiredState, error) {
	root := ""
	if len(envFileRoots) > 0 {
		root = envFileRoots[0]
	}
	var resolveErrs []error
	for i := range files {
		resolved, err := resolveStackEnvFiles(files[i], root)
		if err != nil {
			resolveErrs = append(resolveErrs, fmt.Errorf("stack %q: %w", files[i].Name, err))
			continue
		}
		files[i] = resolved
	}
	if len(resolveErrs) > 0 {
		return DesiredState{}, errors.Join(resolveErrs...)
	}
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

// checkAllowedFields walks the raw YAML for fields outside the supported surface.
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
		key, value := root.Content[i], resolveAlias(root.Content[i+1])
		switch {
		case strings.HasPrefix(key.Value, "x-"):
			// Extension field: inert to the loader, commonly the anchor
			// definition for an alias used elsewhere in the document.
		case !allowedTopLevel[key.Value]:
			errs = append(errs, fmt.Errorf("unsupported compose field %q in stack %q", key.Value, stack))
		case key.Value == "services" && value.Kind == yaml.MappingNode:
			errs = append(errs, checkServices(stack, value)...)
		case (key.Value == "configs" || key.Value == "secrets") && value.Kind == yaml.MappingNode:
			errs = append(errs, checkExternalObjects(stack, key.Value, value)...)
		case key.Value == "volumes" && value.Kind == yaml.MappingNode:
			errs = append(errs, checkVolumeDeclarations(stack, value)...)
		}
	}
	return errs
}

// resolveAlias follows a YAML alias node to the anchor it references, so a
// field validated by Kind (a mapping, a sequence) still validates correctly
// however it was referenced. Non-alias nodes pass through unchanged.
func resolveAlias(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		return n.Alias
	}
	return n
}

// checkExternalObjects validates a top-level configs:/secrets: block: every
// named entry must be external only (no inline file/content/environment),
// so a stack file can reference a cluster object but never define one or
// carry secret material through git.
func checkExternalObjects(stack, kind string, objects *yaml.Node) []error {
	var errs []error
	for i := 0; i+1 < len(objects.Content); i += 2 {
		name, body := objects.Content[i].Value, resolveAlias(objects.Content[i+1])
		if body.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(body.Content); j += 2 {
			sub, subValue := body.Content[j].Value, resolveAlias(body.Content[j+1])
			if !allowedExternalObject[sub] {
				errs = append(errs, fmt.Errorf("unsupported compose field %q in stack %q: %s %q must be external only", kind+"."+sub, stack, kind, name))
				continue
			}
			if sub == "external" && subValue.Value != "true" {
				errs = append(errs, fmt.Errorf("%s %q in stack %q: external must be true", kind, name, stack))
			}
		}
	}
	return errs
}

// checkVolumeDeclarations validates a top-level volumes: block: every named
// entry must be a bare declaration (no driver, driver_opts, external, or
// labels) — named, local-driver volumes only, with no configuration surface
// to validate.
func checkVolumeDeclarations(stack string, volumes *yaml.Node) []error {
	var errs []error
	for i := 0; i+1 < len(volumes.Content); i += 2 {
		name, body := volumes.Content[i].Value, resolveAlias(volumes.Content[i+1])
		if body.Kind == yaml.MappingNode && len(body.Content) > 0 {
			errs = append(errs, fmt.Errorf("unsupported compose field \"volumes.%s\" in stack %q: only a bare declaration is supported (named local-driver volumes only)", name, stack))
		}
	}
	return errs
}

func checkServices(stack string, services *yaml.Node) []error {
	var errs []error
	for i := 0; i+1 < len(services.Content); i += 2 {
		name, body := services.Content[i].Value, resolveAlias(services.Content[i+1])
		if body.Kind != yaml.MappingNode {
			continue
		}
		qualified := ServiceName(stack, name)
		for j := 0; j+1 < len(body.Content); j += 2 {
			key, value := body.Content[j], resolveAlias(body.Content[j+1])
			switch {
			case !allowedService[key.Value]:
				errs = append(errs, fmt.Errorf("unsupported compose field %q in service %q", key.Value, qualified))
			case key.Value == "env_file":
				// Resolved to a host-side file before compose loading.
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
					item = resolveAlias(item)
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
			case (key.Value == "configs" || key.Value == "secrets") && value.Kind == yaml.SequenceNode:
				for _, item := range value.Content {
					item = resolveAlias(item)
					if item.Kind != yaml.MappingNode {
						// Short syntax (bare name, no target override): no subkeys.
						continue
					}
					for k := 0; k+1 < len(item.Content); k += 2 {
						if sub := item.Content[k].Value; !allowedFileRef[sub] {
							errs = append(errs, fmt.Errorf("unsupported compose field %q in service %q", key.Value+"."+sub, qualified))
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
		volumes, err := serviceVolumes(s.Volumes)
		if err != nil {
			return fmt.Errorf("service %q: %w", key, err)
		}
		configs, err := serviceFileRefs(configRefsToFileRefs(s.Configs), mapKeys(project.Configs), "config")
		if err != nil {
			return fmt.Errorf("service %q: %w", key, err)
		}
		secrets, err := serviceFileRefs(secretRefsToFileRefs(s.Secrets), mapKeys(project.Secrets), "secret")
		if err != nil {
			return fmt.Errorf("service %q: %w", key, err)
		}
		ulimits, err := serviceUlimits(s.Ulimits)
		if err != nil {
			return fmt.Errorf("service %q: %w", key, err)
		}
		mode, err := deployMode(s.Deploy)
		if err != nil {
			return fmt.Errorf("service %q: %w", key, err)
		}
		resources, err := deployResources(s.Deploy)
		if err != nil {
			return fmt.Errorf("service %q: %w", key, err)
		}
		updateConfig, err := deployUpdateConfig(s.Deploy)
		if err != nil {
			return fmt.Errorf("service %q: %w", key, err)
		}

		name := ServiceName(f.Name, key)
		if first, dup := owner[name]; dup && first != f.Name {
			return fmt.Errorf("service %q collides with a service of the same qualified name already defined by stack %q", name, first)
		}
		owner[name] = f.Name
		spec := Normalize(f.Name, ServiceSpec{
			Name:            name,
			Image:           s.Image,
			Replicas:        DefaultReplicas(deployReplicas(s.Deploy)),
			Env:             serviceEnv(s.Environment),
			Labels:          s.Labels,
			Networks:        serviceNetworks(project, s),
			Ports:           ports,
			Healthcheck:     healthcheck,
			Command:         []string(s.Command),
			Entrypoint:      []string(s.Entrypoint),
			Hostname:        s.Hostname,
			CapAdd:          s.CapAdd,
			User:            s.User,
			StopGracePeriod: serviceStopGracePeriod(s.StopGracePeriod),
			Ulimits:         ulimits,
			Volumes:         volumes,
			Configs:         configs,
			Secrets:         secrets,
			Mode:            mode,
			RestartPolicy:   deployRestartPolicy(s.Deploy),
			Resources:       resources,
			Placement:       deployPlacement(s.Deploy),
			UpdateConfig:    updateConfig,
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
	case len(s.Volumes) > maxVolumes:
		return fmt.Errorf("service %q: %d volumes exceeds the %d limit", name, len(s.Volumes), maxVolumes)
	case len(s.Configs) > maxConfigs:
		return fmt.Errorf("service %q: %d configs exceeds the %d limit", name, len(s.Configs), maxConfigs)
	case len(s.Secrets) > maxSecrets:
		return fmt.Errorf("service %q: %d secrets exceeds the %d limit", name, len(s.Secrets), maxSecrets)
	case len(s.CapAdd) > maxCapAdd:
		return fmt.Errorf("service %q: %d cap_add entries exceeds the %d limit", name, len(s.CapAdd), maxCapAdd)
	case len(s.Ulimits) > maxUlimits:
		return fmt.Errorf("service %q: %d ulimits exceeds the %d limit", name, len(s.Ulimits), maxUlimits)
	case len(s.Command) > maxCommandArgs:
		return fmt.Errorf("service %q: %d command arguments exceeds the %d limit", name, len(s.Command), maxCommandArgs)
	case len(s.Entrypoint) > maxCommandArgs:
		return fmt.Errorf("service %q: %d entrypoint arguments exceeds the %d limit", name, len(s.Entrypoint), maxCommandArgs)
	case s.Placement != nil && len(s.Placement.Constraints) > maxPlacementConstraints:
		return fmt.Errorf("service %q: %d placement constraints exceeds the %d limit", name, len(s.Placement.Constraints), maxPlacementConstraints)
	default:
		return nil
	}
}

// mapKeys returns the key set of m, discarding values — used where only
// membership (does a service-level reference name a declared top-level
// object?) matters.
func mapKeys[K comparable, V any](m map[K]V) map[K]bool {
	out := make(map[K]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// rawFileRef is the common shape of a compose-go service-level config or
// secret reference, after discarding the uid/gid/mode fields that
// checkAllowedFields already refused to let through (allowedFileRef permits
// only source/target).
type rawFileRef struct{ Source, Target string }

func configRefsToFileRefs(in []types.ServiceConfigObjConfig) []rawFileRef {
	out := make([]rawFileRef, len(in))
	for i, c := range in {
		out[i] = rawFileRef{Source: c.Source, Target: c.Target}
	}
	return out
}

func secretRefsToFileRefs(in []types.ServiceSecretConfig) []rawFileRef {
	out := make([]rawFileRef, len(in))
	for i, c := range in {
		out[i] = rawFileRef{Source: c.Source, Target: c.Target}
	}
	return out
}

// serviceFileRefs converts a service's config/secret references into normal
// form, defaulting an omitted Target to /<kind>s/<source> (the engine's own
// default mount path), and rejecting a reference to a name with no matching
// top-level declaration — the only place its external:true-only shape is
// asserted, since checkExternalObjects already refused any other shape.
func serviceFileRefs(refs []rawFileRef, declared map[string]bool, kind string) ([]FileRef, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]FileRef, 0, len(refs))
	for _, r := range refs {
		if !declared[r.Source] {
			return nil, fmt.Errorf("%s %q is not declared as an external top-level %s", r.Source, kind, kind)
		}
		target := r.Target
		if target == "" {
			target = "/" + kind + "s/" + r.Source
		}
		out = append(out, FileRef{Source: r.Source, Target: target})
	}
	return out, nil
}

// serviceVolumes converts named, local-driver volume mounts into normal
// form, rejecting bind mounts, tmpfs, image mounts, and anything else
// outside that shape.
func serviceVolumes(in []types.ServiceVolumeConfig) ([]VolumeMount, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]VolumeMount, 0, len(in))
	for _, v := range in {
		if v.Type != "volume" || v.Bind != nil || v.Tmpfs != nil || v.Image != nil {
			return nil, fmt.Errorf("volume %q: only named local-driver volumes are supported", v.Target)
		}
		out = append(out, VolumeMount{Source: v.Source, Target: v.Target, ReadOnly: v.ReadOnly})
	}
	return out, nil
}

// serviceUlimits converts the ulimit map into normal form. A shorthand
// single value (`nofile: 1024`) sets both soft and hard to the same limit.
func serviceUlimits(in map[string]*types.UlimitsConfig) ([]UlimitSpec, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]UlimitSpec, 0, len(in))
	for name, u := range in {
		soft, hard := int64(u.Soft), int64(u.Hard)
		if u.Single != 0 {
			soft, hard = int64(u.Single), int64(u.Single)
		}
		out = append(out, UlimitSpec{Name: name, Soft: soft, Hard: hard})
	}
	return out, nil
}

// deployMode extracts deploy.mode, validating it against the modes Swarm
// accepts. A nil deploy block or an empty mode both mean "unspecified";
// Normalize fills in the "replicated" default.
func deployMode(d *types.DeployConfig) (string, error) {
	if d == nil || d.Mode == "" {
		return "", nil
	}
	if !deployModes[d.Mode] {
		return "", fmt.Errorf("deploy.mode %q is not one of replicated, global, replicated-job, global-job", d.Mode)
	}
	return d.Mode, nil
}

// deployRestartPolicy extracts deploy.restart_policy. A nil policy leaves
// the field nil in normal form, letting the engine default apply.
func deployRestartPolicy(d *types.DeployConfig) *RestartPolicySpec {
	if d == nil || d.RestartPolicy == nil {
		return nil
	}
	rp := d.RestartPolicy
	out := &RestartPolicySpec{Condition: rp.Condition}
	if rp.Delay != nil {
		out.Delay = time.Duration(*rp.Delay)
	}
	if rp.MaxAttempts != nil {
		out.MaxAttempts = *rp.MaxAttempts
	}
	if rp.Window != nil {
		out.Window = time.Duration(*rp.Window)
	}
	return out
}

// deployResources extracts deploy.resources.limits. Reservations are not
// modelled. A resources block with no memory limit is rejected outright:
// an unbounded service is exactly the resource-exhaustion risk the caps in
// this file exist to prevent, and CPU-only limiting does not guard against
// it.
func deployResources(d *types.DeployConfig) (*ResourcesSpec, error) {
	if d == nil || d.Resources.Limits == nil {
		return nil, nil
	}
	limits := d.Resources.Limits
	if limits.MemoryBytes == 0 {
		return nil, errors.New("deploy.resources.limits.memory is required when resources.limits is set")
	}
	return &ResourcesSpec{
		MemoryBytes: int64(limits.MemoryBytes),
		NanoCPUs:    int64(limits.NanoCPUs * 1e9),
	}, nil
}

// deployPlacement extracts deploy.placement.constraints. Preferences and
// max_replicas_per_node are not modelled.
func deployPlacement(d *types.DeployConfig) *PlacementSpec {
	if d == nil || len(d.Placement.Constraints) == 0 {
		return nil
	}
	return &PlacementSpec{Constraints: append([]string(nil), d.Placement.Constraints...)}
}

// deployUpdateConfig extracts deploy.update_config.parallelism. A present
// update_config with no explicit positive parallelism is rejected: compose
// treats an omitted or zero value as "unlimited," which for a Swarm rolling
// update means every task restarts at once — the opposite of what
// update_config is for.
func deployUpdateConfig(d *types.DeployConfig) (*UpdateConfigSpec, error) {
	if d == nil || d.UpdateConfig == nil {
		return nil, nil
	}
	if d.UpdateConfig.Parallelism == nil || *d.UpdateConfig.Parallelism == 0 {
		return nil, errors.New("deploy.update_config.parallelism must be set and greater than zero")
	}
	return &UpdateConfigSpec{Parallelism: *d.UpdateConfig.Parallelism}, nil
}

// serviceStopGracePeriod converts an optional stop_grace_period to a
// time.Duration, zero when unset.
func serviceStopGracePeriod(d *types.Duration) time.Duration {
	if d == nil {
		return 0
	}
	return time.Duration(*d)
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
			// ranges; those are unsupported, so anything non-numeric fails.
			n, err := strconv.ParseUint(p.Published, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("published port %q: %w", p.Published, err)
			}
			published = uint32(n)
		}
		ports = append(ports, PortSpec{
			Target:    p.Target,
			Published: published,
			Protocol:  p.Protocol,
			Mode:      p.Mode,
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
