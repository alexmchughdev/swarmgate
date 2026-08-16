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
	// Changed lists the differing fields in a fixed order:
	// image, replicas, env, labels, networks, ports, healthcheck.
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
// with any FR4 field difference in Updates.
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
	return changed
}

func sortedKeys(m map[string]spec.ServiceSpec) []string {
	return slices.Sorted(maps.Keys(m))
}
