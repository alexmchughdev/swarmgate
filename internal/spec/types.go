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
}

// PortSpec is one published port.
type PortSpec struct {
	Target    uint32
	Published uint32
	Protocol  string
}

// HealthcheckSpec mirrors the container healthcheck a service declares.
type HealthcheckSpec struct {
	Test        []string
	Interval    time.Duration
	Timeout     time.Duration
	Retries     int
	StartPeriod time.Duration
}
