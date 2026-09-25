package spec

import "time"

// ManagedLabel marks a service as owned by swarmgate. It is set on every
// apply and is the guard the pruner checks before removing anything.
const ManagedLabel = "swarmgate.managed"

// LabelPrefix scopes the labels swarmgate considers part of a service's
// normal form; all other labels are ignored on both sides of the diff.
const LabelPrefix = "swarmgate."

// StackLabel records which stack a service belongs to. Deriving the stack
// from the service name is ambiguous once stack names contain underscores,
// so membership is carried explicitly on the service itself.
const StackLabel = "swarmgate.stack"

// DesiredState is the full set of services the Git source asks for,
// keyed by qualified service name (see ServiceName).
type DesiredState struct {
	Services map[string]ServiceSpec
}

// ObservedState is the set of swarmgate-managed services currently in the
// cluster, in the same normal form and keyed the same way as DesiredState.
type ObservedState struct {
	Services map[string]ServiceSpec
}

// ServiceSpec is the normal form of one service. Both the compose parser
// and the cluster observer produce this shape, so a field-by-field
// comparison is meaningful and free of phantom diffs.
type ServiceSpec struct {
	// Name is the qualified service name <stack>_<service>; it always
	// equals the map key under which the spec is stored.
	Name string

	// Image is the reference as given by the source until the resolve
	// stage pins it, after which it is repo@sha256:... form.
	Image string

	Replicas uint64

	// Env holds environment variables in map form. Compared in full: the
	// observed side is the submitted service spec, not runtime container
	// env, so it carries no image-injected keys to tolerate.
	Env map[string]string

	// Labels retains only labels under LabelPrefix; everything else is
	// outside swarmgate's contract and excluded from comparison.
	Labels map[string]string

	// Networks is the sorted list of attached network names, always
	// including the stack default network <stack>_default.
	Networks []string

	// Ports is sorted by (Target, Published, Protocol) with Protocol
	// lowercased, so slice equality is order-insensitive.
	Ports []PortSpec

	// Healthcheck is nil when the service defines none. Its presence
	// drives the converged predicate's health clause.
	Healthcheck *HealthcheckSpec

	// Command overrides the image's CMD; nil leaves it untouched.
	Command []string
	// Entrypoint overrides the image's ENTRYPOINT; nil leaves it untouched.
	Entrypoint []string
	// Hostname is carried verbatim, including any Swarm task-template
	// placeholder such as "{{.Node.Hostname}}" — it is never interpolated
	// by swarmgate; the engine resolves it per task.
	Hostname string
	// CapAdd is a sorted, deduplicated set of added Linux capabilities.
	CapAdd []string
	User   string
	// StopGracePeriod is zero when unset, letting the engine default apply.
	StopGracePeriod time.Duration
	// Ulimits is sorted by Name for stable comparison.
	Ulimits []UlimitSpec
	// Volumes holds named, local-driver volume mounts only; sorted by
	// Target. Bind mounts, tmpfs, and non-local drivers are outside the
	// normal form and rejected at parse.
	Volumes []VolumeMount
	// Configs and Secrets reference cluster objects that must already
	// exist (external only — swarmgate never creates or reads their
	// content); both are sorted by Target.
	Configs []FileRef
	Secrets []FileRef

	// Mode is the deploy mode: "replicated", "global", "replicated-job",
	// or "global-job". Normal form always carries an explicit value —
	// "replicated" when compose omits deploy.mode entirely.
	Mode          string
	RestartPolicy *RestartPolicySpec
	Resources     *ResourcesSpec
	Placement     *PlacementSpec
	UpdateConfig  *UpdateConfigSpec
}

// PortSpec is one published port.
type PortSpec struct {
	Target    uint32
	Published uint32
	Protocol  string
	// Mode is the publish mode, "host" or "ingress". Normal form always
	// carries an explicit value — "ingress" when compose omits it — so it
	// round-trips through the diff instead of silently collapsing to
	// whatever the engine happens to default to.
	Mode string
}

// UlimitSpec is one container ulimit.
type UlimitSpec struct {
	Name string
	Soft int64
	Hard int64
}

// VolumeMount is one named, local-driver volume attachment.
type VolumeMount struct {
	Source   string // the named volume
	Target   string // mount path inside the container
	ReadOnly bool
}

// FileRef is a reference to an externally managed config or secret object,
// by name, and the path it is mounted at.
type FileRef struct {
	Source string // the config/secret name
	Target string // mount path inside the container
}

// RestartPolicySpec mirrors deploy.restart_policy.
type RestartPolicySpec struct {
	Condition   string // "none", "on-failure", or "any"
	Delay       time.Duration
	MaxAttempts uint64
	Window      time.Duration
}

// ResourcesSpec mirrors deploy.resources.limits. Reservations and
// non-trivial resource types (devices, generic resources) are outside the
// normal form.
type ResourcesSpec struct {
	MemoryBytes int64
	NanoCPUs    int64
}

// PlacementSpec mirrors deploy.placement. Preferences and
// max_replicas_per_node are outside the normal form; only constraints are
// modelled.
type PlacementSpec struct {
	Constraints []string
}

// UpdateConfigSpec mirrors deploy.update_config. Only parallelism is
// modelled.
type UpdateConfigSpec struct {
	Parallelism uint64
}

// HealthcheckSpec mirrors the container healthcheck a service declares.
type HealthcheckSpec struct {
	Test        []string
	Interval    time.Duration
	Timeout     time.Duration
	Retries     int
	StartPeriod time.Duration
}
