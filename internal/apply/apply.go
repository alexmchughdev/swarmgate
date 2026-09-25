package apply

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"

	"github.com/alexmchughdev/swarmgate/internal/spec"
)

// Action names one reconciliation verb. The string values double as the
// telemetry apply.action attribute values and must not change.
type Action string

const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionRemove Action = "remove"
)

// Change is one ordered unit of work handed to an Applier. The loop
// translates diff.Diff into these, which keeps the applier a dumb executor
// with no dependency on the differ.
type Change struct {
	Action Action
	Spec   spec.ServiceSpec
	// Recreate pairs the config/secret-only replacement remove and create
	// operations so the loop can preserve their dependency and await only
	// the final created state.
	Recreate bool
}

// Applier executes a single change against the cluster. Implementations
// perform no retries: retry policy is owned by the loop.
type Applier interface {
	Apply(ctx context.Context, c Change) error
}

// serviceAPI is the slice of the Docker client the applier actually uses,
// kept narrow so tests can substitute it without a live daemon.
type serviceAPI interface {
	ServiceCreate(ctx context.Context, service swarm.ServiceSpec, options swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error)
	ServiceUpdate(ctx context.Context, serviceID string, version swarm.Version, service swarm.ServiceSpec, options swarm.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error)
	ServiceInspectWithRaw(ctx context.Context, serviceID string, opts swarm.ServiceInspectOptions) (swarm.Service, []byte, error)
	ServiceRemove(ctx context.Context, serviceID string) error
	NetworkList(ctx context.Context, options network.ListOptions) ([]network.Summary, error)
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkInspect(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, error)
	ConfigList(ctx context.Context, options swarm.ConfigListOptions) ([]swarm.Config, error)
	SecretList(ctx context.Context, options swarm.SecretListOptions) ([]swarm.Secret, error)
}

// SwarmApplier applies changes to a live Swarm manager.
type SwarmApplier struct {
	api serviceAPI
}

var _ Applier = (*SwarmApplier)(nil)

// NewSwarmApplier connects to the Docker engine at host, or via the
// standard environment when host is empty.
func NewSwarmApplier(host string) (*SwarmApplier, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	c, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &SwarmApplier{api: c}, nil
}

// Apply dispatches on the change's action.
func (a *SwarmApplier) Apply(ctx context.Context, c Change) error {
	switch c.Action {
	case ActionCreate:
		return a.create(ctx, c.Spec)
	case ActionUpdate:
		return a.update(ctx, c.Spec)
	case ActionRemove:
		return a.remove(ctx, c.Spec)
	default:
		return fmt.Errorf("apply %q: unknown action %q", c.Spec.Name, c.Action)
	}
}

func (a *SwarmApplier) create(ctx context.Context, s spec.ServiceSpec) error {
	// The managed label is the prune guard: a service created without it
	// could never be removed by swarmgate again. Normal form always stamps
	// it, so a miss means a bug upstream; fail closed rather than create
	// an unremovable orphan.
	if s.Labels[spec.ManagedLabel] != "true" {
		return fmt.Errorf("refusing to create %q without %s label", s.Name, spec.ManagedLabel)
	}
	ids, err := a.ensureNetworks(ctx, s)
	if err != nil {
		return fmt.Errorf("create %q: %w", s.Name, err)
	}
	desired := spec.ToSwarm(s)
	attachByID(&desired, ids)
	if err := a.attachConfigsAndSecrets(ctx, &desired, s); err != nil {
		return fmt.Errorf("create %q: %w", s.Name, err)
	}
	if _, err := a.api.ServiceCreate(ctx, desired, swarm.ServiceCreateOptions{}); err != nil {
		return fmt.Errorf("create %q: %w", s.Name, err)
	}
	return nil
}

func (a *SwarmApplier) update(ctx context.Context, s spec.ServiceSpec) error {
	current, _, err := a.api.ServiceInspectWithRaw(ctx, s.Name, swarm.ServiceInspectOptions{})
	if err != nil {
		return fmt.Errorf("update %q: inspect: %w", s.Name, err)
	}
	if err := checkImmutableChange(current.Spec, s); err != nil {
		return fmt.Errorf("update %q: %w", s.Name, err)
	}
	ids, err := a.ensureNetworks(ctx, s)
	if err != nil {
		return fmt.Errorf("update %q: %w", s.Name, err)
	}

	// Start from the inspected spec and overwrite only the modelled
	// fields: everything the model does not cover (engine-managed
	// settings, operator-set options like grace periods or placement)
	// must survive an update untouched.
	desired := spec.ToSwarm(s)
	attachByID(&desired, ids)
	if err := a.attachConfigsAndSecrets(ctx, &desired, s); err != nil {
		return fmt.Errorf("update %q: %w", s.Name, err)
	}
	mutated := current.Spec

	mutated.Annotations.Name = desired.Annotations.Name
	mutated.Annotations.Labels = mergeLabels(mutated.Annotations.Labels, desired.Annotations.Labels)

	// Copy the container spec so the inspected value is never mutated
	// through its pointer.
	cs := swarm.ContainerSpec{}
	if mutated.TaskTemplate.ContainerSpec != nil {
		cs = *mutated.TaskTemplate.ContainerSpec
	}
	cs.Image = desired.TaskTemplate.ContainerSpec.Image
	cs.Env = desired.TaskTemplate.ContainerSpec.Env
	cs.Healthcheck = desired.TaskTemplate.ContainerSpec.Healthcheck
	cs.Command = desired.TaskTemplate.ContainerSpec.Command
	cs.Args = desired.TaskTemplate.ContainerSpec.Args
	cs.Hostname = desired.TaskTemplate.ContainerSpec.Hostname
	cs.User = desired.TaskTemplate.ContainerSpec.User
	cs.CapabilityAdd = desired.TaskTemplate.ContainerSpec.CapabilityAdd
	cs.StopGracePeriod = desired.TaskTemplate.ContainerSpec.StopGracePeriod
	cs.Ulimits = desired.TaskTemplate.ContainerSpec.Ulimits
	cs.Mounts = desired.TaskTemplate.ContainerSpec.Mounts
	// Configs/Secrets are re-attached, not diffed here: checkImmutableChange
	// above has already refused this call if the referenced set actually
	// changed, so this is either a no-op re-assertion or a same-set
	// resolve-to-ID pass.
	cs.Configs = desired.TaskTemplate.ContainerSpec.Configs
	cs.Secrets = desired.TaskTemplate.ContainerSpec.Secrets
	mutated.TaskTemplate.ContainerSpec = &cs

	mutated.Mode = desired.Mode
	mutated.TaskTemplate.Networks = desired.TaskTemplate.Networks
	mutated.TaskTemplate.RestartPolicy = desired.TaskTemplate.RestartPolicy
	mutated.TaskTemplate.Resources = desired.TaskTemplate.Resources
	mutated.TaskTemplate.Placement = desired.TaskTemplate.Placement
	mutated.UpdateConfig = desired.UpdateConfig

	// Only Ports is modelled; the rest of the endpoint spec (resolution
	// mode) is carried over.
	var ports []swarm.PortConfig
	if desired.EndpointSpec != nil {
		ports = desired.EndpointSpec.Ports
	}
	if mutated.EndpointSpec != nil {
		ep := *mutated.EndpointSpec
		ep.Ports = ports
		mutated.EndpointSpec = &ep
	} else if len(ports) > 0 {
		mutated.EndpointSpec = &swarm.EndpointSpec{Ports: ports}
	}

	if _, err := a.api.ServiceUpdate(ctx, current.ID, current.Version, mutated, swarm.ServiceUpdateOptions{}); err != nil {
		return fmt.Errorf("update %q: %w", s.Name, err)
	}
	return nil
}

// checkImmutableChange refuses an update that would change the set of
// attached configs or secrets. Swarm does not support swapping a running
// task's config/secret attachments in place — the new content only reaches
// a container that Swarm creates fresh, which a ServiceUpdate is not
// guaranteed to do (an otherwise-unchanged task is left running). Remove
// and recreate the service instead, so the engine builds the container from
// scratch with the new attachment set.
func checkImmutableChange(current swarm.ServiceSpec, desired spec.ServiceSpec) error {
	var currentConfigs, currentSecrets []string
	if cs := current.TaskTemplate.ContainerSpec; cs != nil {
		for _, c := range cs.Configs {
			currentConfigs = append(currentConfigs, c.ConfigName+":"+refFileName(c.File))
		}
		for _, s := range cs.Secrets {
			currentSecrets = append(currentSecrets, s.SecretName+":"+refFileNameSecret(s.File))
		}
	}
	slices.Sort(currentConfigs)
	slices.Sort(currentSecrets)

	desiredConfigs := fileRefKeys(desired.Configs)
	desiredSecrets := fileRefKeys(desired.Secrets)

	if !slices.Equal(currentConfigs, desiredConfigs) {
		return errors.New("configs changed: Swarm cannot update a running service's config attachments in place; remove and recreate the service")
	}
	if !slices.Equal(currentSecrets, desiredSecrets) {
		return errors.New("secrets changed: Swarm cannot update a running service's secret attachments in place; remove and recreate the service")
	}
	return nil
}

func fileRefKeys(refs []spec.FileRef) []string {
	keys := make([]string, len(refs))
	for i, r := range refs {
		keys[i] = r.Source + ":" + r.Target
	}
	slices.Sort(keys)
	return keys
}

func refFileName(f *swarm.ConfigReferenceFileTarget) string {
	if f == nil {
		return ""
	}
	return f.Name
}

func refFileNameSecret(f *swarm.SecretReferenceFileTarget) string {
	if f == nil {
		return ""
	}
	return f.Name
}

// mergeLabels returns current with every swarmgate.*-prefixed key
// replaced by desired's, and every other key preserved untouched. Labels
// outside that prefix are not part of swarmgate's model (Normalize never
// puts them in desired), so replacing the whole map wholesale destroyed
// anything an operator set directly, such as a label another tool (a
// proxy, a monitoring agent) reads from the live service. Removing the
// prefix wholesale before re-adding desired's still lets a stack file
// drop a swarmgate.* label it used to set.
func mergeLabels(current, desired map[string]string) map[string]string {
	merged := make(map[string]string, len(current)+len(desired))
	for k, v := range current {
		if !strings.HasPrefix(k, spec.LabelPrefix) {
			merged[k] = v
		}
	}
	for k, v := range desired {
		merged[k] = v
	}
	return merged
}

func (a *SwarmApplier) remove(ctx context.Context, s spec.ServiceSpec) error {
	current, _, err := a.api.ServiceInspectWithRaw(ctx, s.Name, swarm.ServiceInspectOptions{})
	if err != nil {
		return fmt.Errorf("remove %q: inspect: %w", s.Name, err)
	}
	// Blast-radius guard: never delete a service swarmgate does not own,
	// whatever the loop asked for. Fail closed on a missing label.
	if current.Spec.Annotations.Labels[spec.ManagedLabel] != "true" {
		return fmt.Errorf("remove %q: refusing to remove service without %s=true", s.Name, spec.ManagedLabel)
	}
	if err := a.api.ServiceRemove(ctx, current.ID); err != nil {
		return fmt.Errorf("remove %q: %w", s.Name, err)
	}
	return nil
}

// ensureNetworks creates any referenced network that does not exist yet and
// returns the name-to-ID mapping for every referenced network. Services
// deployed via `docker stack deploy` get their stack networks created
// implicitly by the CLI; swarmgate owns that responsibility itself. Created
// networks are overlay (the only swarm-scoped driver) and carry the managed
// label; pre-existing networks are used as-is and never modified.
func (a *SwarmApplier) ensureNetworks(ctx context.Context, s spec.ServiceSpec) (map[string]string, error) {
	ids := make(map[string]string, len(s.Networks))
	for _, name := range s.Networks {
		list, err := a.api.NetworkList(ctx, network.ListOptions{
			Filters: filters.NewArgs(filters.Arg("name", name)),
		})
		if err != nil {
			return nil, fmt.Errorf("list networks: %w", err)
		}
		// The name filter matches substrings; require an exact hit.
		for _, n := range list {
			if n.Name == name {
				ids[name] = n.ID
				break
			}
		}
		if _, ok := ids[name]; ok {
			continue
		}
		created, err := a.api.NetworkCreate(ctx, name, network.CreateOptions{
			Driver: "overlay",
			Labels: map[string]string{spec.ManagedLabel: "true"},
		})
		if err != nil {
			return nil, fmt.Errorf("create network %q: %w", name, err)
		}
		if err := a.awaitNetwork(ctx, created.ID); err != nil {
			return nil, fmt.Errorf("create network %q: %w", name, err)
		}
		ids[name] = created.ID
	}
	return ids, nil
}

// attachConfigsAndSecrets resolves every config/secret ToSwarm referenced by
// name onto its live object ID, the same name-to-ID problem ensureNetworks
// solves for networks — except configs and secrets are external:true only
// (see internal/spec's parse allowlist), so unlike ensureNetworks this
// never creates one: a reference to a name with no matching live object is
// an error, since swarmgate is not the thing that was supposed to create it.
func (a *SwarmApplier) attachConfigsAndSecrets(ctx context.Context, desired *swarm.ServiceSpec, s spec.ServiceSpec) error {
	cs := desired.TaskTemplate.ContainerSpec
	if len(s.Configs) > 0 {
		configs, err := a.api.ConfigList(ctx, swarm.ConfigListOptions{})
		if err != nil {
			return fmt.Errorf("list configs: %w", err)
		}
		byName := make(map[string]string, len(configs))
		for _, c := range configs {
			byName[c.Spec.Name] = c.ID
		}
		for _, ref := range cs.Configs {
			id, ok := byName[ref.ConfigName]
			if !ok {
				return fmt.Errorf("config %q not found: external configs must already exist in the cluster", ref.ConfigName)
			}
			ref.ConfigID = id
		}
	}
	if len(s.Secrets) > 0 {
		secrets, err := a.api.SecretList(ctx, swarm.SecretListOptions{})
		if err != nil {
			return fmt.Errorf("list secrets: %w", err)
		}
		byName := make(map[string]string, len(secrets))
		for _, sec := range secrets {
			byName[sec.Spec.Name] = sec.ID
		}
		for _, ref := range cs.Secrets {
			id, ok := byName[ref.SecretName]
			if !ok {
				return fmt.Errorf("secret %q not found: external secrets must already exist in the cluster", ref.SecretName)
			}
			ref.SecretID = id
		}
	}
	return nil
}

// awaitNetwork blocks until a just-created network is readable. Swarm-scope
// network creation is asynchronous: NetworkCreate returns before the network
// is visible in the cluster store, and a ServiceCreate referencing it in that
// window fails with not-found. This is a bounded consistency barrier on our
// own write, not a retry policy (which stays with the loop).
func (a *SwarmApplier) awaitNetwork(ctx context.Context, id string) error {
	const (
		interval = 100 * time.Millisecond
		attempts = 100
	)
	var lastErr error
	for i := 0; i < attempts; i++ {
		_, err := a.api.NetworkInspect(ctx, id, network.InspectOptions{})
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("network %s not visible after create: %w", id, lastErr)
}

// attachByID rewrites network attachment targets from names to IDs. A name
// only becomes resolvable by the engine after swarm state propagates, so a
// service created immediately after its network intermittently fails
// name-based lookup; IDs are valid the moment NetworkCreate returns, which
// is also why `docker stack deploy` attaches by ID.
func attachByID(sw *swarm.ServiceSpec, ids map[string]string) {
	for i, n := range sw.TaskTemplate.Networks {
		if id, ok := ids[n.Target]; ok {
			sw.TaskTemplate.Networks[i].Target = id
		}
	}
}
