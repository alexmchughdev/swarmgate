package spec

import (
	"fmt"
	"slices"
	"strings"
)

// ServiceName returns the qualified service name for a compose service key
// within a stack.
//
// Rule: service name = <stack>_<service>, matching the naming convention
// of `docker stack deploy` so swarmgate-managed services are addressable
// with familiar names and observed cluster names map back losslessly.
func ServiceName(stack, service string) string {
	return fmt.Sprintf("%s_%s", stack, service)
}

// DefaultNetwork returns the name of a stack's default network.
//
// Rule: the stack default network is spelled out explicitly as
// <stack>_default in normal form. Leaving it implicit on one side of the
// comparison and explicit on the other would produce a phantom diff on
// every service that uses only the default network.
func DefaultNetwork(stack string) string {
	return stack + "_default"
}

// DefaultReplicas applies the replica default at parse time.
//
// Rule: replicas default to 1 when compose omits deploy.replicas. The
// default is applied where omission is still distinguishable from an
// explicit zero (a valid scale-to-zero request), which is why it takes a
// pointer rather than defaulting a zero value blindly.
func DefaultReplicas(r *uint64) uint64 {
	if r == nil {
		return 1
	}
	return *r
}

// Normalize returns s rewritten into normal form for the given stack.
// Both the compose parser and the cluster observer must route their output
// through this single function; the rules below exist to kill phantom
// diffs between the two sides.
func Normalize(stack string, s ServiceSpec) ServiceSpec {
	// Rule (env): environment is canonicalised to map form and nothing is
	// dropped. The differ compares Env in full.
	//
	// Rule (empties): empty collections are nil in normal form. The two
	// sides build their specs differently (parser allocates, observer
	// appends), and reflect.DeepEqual distinguishes nil from empty — a
	// class of phantom mismatch this rule eliminates.
	if len(s.Env) == 0 {
		s.Env = nil
	}
	if len(s.Ports) == 0 {
		s.Ports = nil
	}

	// Rule (image): the image reference is stored as given. Digest pinning
	// happens in the resolve stage, after which Image is repo@sha256:...;
	// normalisation must not touch it in either state.

	// Rule (labels): only labels under LabelPrefix participate in the
	// normal form; anything else (orchestrator bookkeeping, operator
	// annotations) is outside swarmgate's contract. ManagedLabel is always
	// present and true: it is set on every apply and doubles as the prune
	// guard, so both sides of the diff must carry it identically. StackLabel
	// is likewise always present so observed services map back to their
	// stack without parsing service names.
	labels := make(map[string]string)
	for k, v := range s.Labels {
		if strings.HasPrefix(k, LabelPrefix) {
			labels[k] = v
		}
	}
	labels[ManagedLabel] = "true"
	labels[StackLabel] = stack
	s.Labels = labels

	// Rule (networks): sorted, deduplicated slice of network names so
	// attachment order never diffs. A service with no explicit networks
	// attaches to the stack default network, spelled out explicitly.
	if len(s.Networks) == 0 {
		s.Networks = []string{DefaultNetwork(stack)}
	} else {
		networks := slices.Clone(s.Networks)
		slices.Sort(networks)
		s.Networks = slices.Compact(networks)
	}

	// Rule (healthcheck): a test-less healthcheck normalises to nil. Parse
	// rejects one outright, but this keeps FromSwarm(ToSwarm(s)) == s true
	// for any spec built directly in normal form.
	if s.Healthcheck != nil && len(s.Healthcheck.Test) == 0 {
		s.Healthcheck = nil
	}

	// Rule (ports): protocol lowercased with tcp as the default, then
	// sorted by (Target, Published, Protocol) so publication order never
	// diffs.
	if len(s.Ports) > 0 {
		ports := slices.Clone(s.Ports)
		for i := range ports {
			if ports[i].Protocol == "" {
				ports[i].Protocol = "tcp"
			} else {
				ports[i].Protocol = strings.ToLower(ports[i].Protocol)
			}
		}
		slices.SortFunc(ports, comparePorts)
		s.Ports = ports
	}

	return s
}

func comparePorts(a, b PortSpec) int {
	if a.Target != b.Target {
		if a.Target < b.Target {
			return -1
		}
		return 1
	}
	if a.Published != b.Published {
		if a.Published < b.Published {
			return -1
		}
		return 1
	}
	return strings.Compare(a.Protocol, b.Protocol)
}
