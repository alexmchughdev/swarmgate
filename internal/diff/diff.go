package diff

import (
	"maps"
	"reflect"
	"slices"

	"github.com/alexmchughdev/swarmgate/internal/spec"
)

// Change is one in-place service update, carrying both specs plus the list
// of changed fields for telemetry and drift classification.
type Change struct {
	Name string
	Old  spec.ServiceSpec
	New  spec.ServiceSpec
	// Changed lists the differing fields in a fixed order: image,
	// replicas, env, labels, networks, ports, healthcheck, command,
	// entrypoint, hostname, cap_add, user, stop_grace_period, ulimits,
	// volumes, configs, secrets, mode, restart_policy, resources,
	// placement, update_config.
	Changed []string
}

// Diff is the set of actions that would reconcile observed onto desired.
// Slices are sorted by service name so every consumer sees one
// deterministic order.
type Diff struct {
	Creates []spec.ServiceSpec
	Updates []Change
	// Removes holds observed-not-desired services. It is populated
	// regardless of the prune setting; whether removals are acted on is
	// the loop's decision, not the differ's.
	Removes []spec.ServiceSpec
}

// Empty reports whether the diff requires no action.
func (d Diff) Empty() bool {
	return len(d.Creates) == 0 && len(d.Updates) == 0 && len(d.Removes) == 0
}

// Compute is a pure function of the two states: desired-only services land
// in Creates, observed-only in Removes, and services present on both sides
// with any modelled field difference in Updates.
func Compute(desired spec.DesiredState, observed spec.ObservedState) Diff {
	var d Diff
	for _, name := range sortedKeys(desired.Services) {
		want := desired.Services[name]
		have, ok := observed.Services[name]
		if !ok {
			d.Creates = append(d.Creates, want)
			continue
		}
		if changed := changedFields(want, have); len(changed) > 0 {
			d.Updates = append(d.Updates, Change{Name: name, Old: have, New: want, Changed: changed})
		}
	}
	for _, name := range sortedKeys(observed.Services) {
		if _, ok := desired.Services[name]; !ok {
			d.Removes = append(d.Removes, observed.Services[name])
		}
	}
	return d
}

func changedFields(want, have spec.ServiceSpec) []string {
	var changed []string
	if want.Image != have.Image {
		changed = append(changed, "image")
	}
	if want.Replicas != have.Replicas {
		changed = append(changed, "replicas")
	}
	if !maps.Equal(want.Env, have.Env) {
		changed = append(changed, "env")
	}
	if !maps.Equal(want.Labels, have.Labels) {
		changed = append(changed, "labels")
	}
	if !slices.Equal(want.Networks, have.Networks) {
		changed = append(changed, "networks")
	}
	if !slices.Equal(want.Ports, have.Ports) {
		changed = append(changed, "ports")
	}
	if !reflect.DeepEqual(want.Healthcheck, have.Healthcheck) {
		changed = append(changed, "healthcheck")
	}
	if !slices.Equal(want.Command, have.Command) {
		changed = append(changed, "command")
	}
	if !slices.Equal(want.Entrypoint, have.Entrypoint) {
		changed = append(changed, "entrypoint")
	}
	if want.Hostname != have.Hostname {
		changed = append(changed, "hostname")
	}
	if !slices.Equal(want.CapAdd, have.CapAdd) {
		changed = append(changed, "cap_add")
	}
	if want.User != have.User {
		changed = append(changed, "user")
	}
	if want.StopGracePeriod != have.StopGracePeriod {
		changed = append(changed, "stop_grace_period")
	}
	if !slices.Equal(want.Ulimits, have.Ulimits) {
		changed = append(changed, "ulimits")
	}
	if !slices.Equal(want.Volumes, have.Volumes) {
		changed = append(changed, "volumes")
	}
	if !slices.Equal(want.Configs, have.Configs) {
		changed = append(changed, "configs")
	}
	if !slices.Equal(want.Secrets, have.Secrets) {
		changed = append(changed, "secrets")
	}
	if want.Mode != have.Mode {
		changed = append(changed, "mode")
	}
	if !reflect.DeepEqual(want.RestartPolicy, have.RestartPolicy) {
		changed = append(changed, "restart_policy")
	}
	if !reflect.DeepEqual(want.Resources, have.Resources) {
		changed = append(changed, "resources")
	}
	if !reflect.DeepEqual(want.Placement, have.Placement) {
		changed = append(changed, "placement")
	}
	if !reflect.DeepEqual(want.UpdateConfig, have.UpdateConfig) {
		changed = append(changed, "update_config")
	}
	return changed
}

func sortedKeys(m map[string]spec.ServiceSpec) []string {
	return slices.Sorted(maps.Keys(m))
}
