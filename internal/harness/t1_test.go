package harness

import (
	"reflect"
	"testing"
)

func TestT1PlanFreshStackStartsAtFirstTag(t *testing.T) {
	services, next := t1Plan(3, 1, nil, "", "", nil, false)
	want := []t1Service{
		{Name: "web1", Image: "nginx:1.25-alpine"}, // bumped from index 0 -> 1
		{Name: "web2", Image: "nginx:1.24-alpine"},
		{Name: "web3", Image: "nginx:1.24-alpine"},
	}
	if !reflect.DeepEqual(services, want) {
		t.Fatalf("services = %+v, want %+v", services, want)
	}
	if !reflect.DeepEqual(next, []int{1, 0, 0}) {
		t.Fatalf("next tag index = %v, want [1 0 0]", next)
	}
}

func TestT1PlanBumpsOnlyChangesServices(t *testing.T) {
	services, next := t1Plan(5, 2, []int{0, 0, 0, 0, 0}, "", "", nil, false)
	if services[0].Image != "nginx:1.25-alpine" || services[1].Image != "nginx:1.25-alpine" {
		t.Fatalf("first 2 services not bumped: %+v", services[:2])
	}
	for i := 2; i < 5; i++ {
		if services[i].Image != "nginx:1.24-alpine" {
			t.Fatalf("service %d unexpectedly bumped: %+v", i, services[i])
		}
	}
	if !reflect.DeepEqual(next, []int{1, 1, 0, 0, 0}) {
		t.Fatalf("next tag index = %v, want [1 1 0 0 0]", next)
	}
}

func TestT1PlanCyclesAndWrapsAroundPinnedTags(t *testing.T) {
	tagIndex := []int{0}
	var seen []string
	for i := 0; i < len(t1Tags)+2; i++ {
		services, next := t1Plan(1, 1, tagIndex, "", "", nil, false)
		seen = append(seen, services[0].Image)
		tagIndex = next
	}
	want := []string{
		"nginx:1.25-alpine", "nginx:1.26-alpine", "nginx:1.27-alpine",
		"nginx:1.28-alpine", "nginx:1.24-alpine", // wraps after the 5th tag
		"nginx:1.25-alpine", "nginx:1.26-alpine",
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("tag cycle = %v, want %v", seen, want)
	}
}

func TestT1PlanChangesGreaterThanScaleClampsToScale(t *testing.T) {
	services, next := t1Plan(2, 10, []int{0, 0}, "", "", nil, false)
	for i, s := range services {
		if s.Image != "nginx:1.25-alpine" {
			t.Fatalf("service %d = %+v, want all bumped exactly once despite changes > scale", i, s)
		}
	}
	if !reflect.DeepEqual(next, []int{1, 1}) {
		t.Fatalf("next tag index = %v, want [1 1]", next)
	}
}

func TestT1PlanMismatchedTagIndexTreatedAsFresh(t *testing.T) {
	// A tagIndex sized for a different scale (e.g. a stack that just grew)
	// must not panic or misalign; it resets to a fresh stack instead.
	services, next := t1Plan(3, 1, []int{4, 4}, "", "", nil, false)
	if len(services) != 3 || len(next) != 3 {
		t.Fatalf("services=%v next=%v, want length 3 each", services, next)
	}
	if services[0].Image != "nginx:1.25-alpine" {
		t.Fatalf("service[0] = %+v, want fresh-stack bump", services[0])
	}
}

func TestT1PlanDeterministic(t *testing.T) {
	a, nextA := t1Plan(10, 3, []int{2, 2, 2, 2, 2, 2, 2, 2, 2, 2}, "", "", nil, false)
	b, nextB := t1Plan(10, 3, []int{2, 2, 2, 2, 2, 2, 2, 2, 2, 2}, "", "", nil, false)
	if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(nextA, nextB) {
		t.Fatalf("t1Plan not deterministic for identical input")
	}
}

// TestT1PlanRegistryPrefixesImage is a regression test: a bare nginx:tag
// reference implies Docker Hub to both the engine and swarmgate's own
// resolve stage, so every resolve during a campaign would be a real WAN
// round trip unless the harness can route through a local mirror instead.
func TestT1PlanRegistryPrefixesImage(t *testing.T) {
	services, _ := t1Plan(2, 2, nil, "192.168.10.13:5000", "", nil, false)
	want := []t1Service{
		{Name: "web1", Image: "192.168.10.13:5000/nginx:1.25-alpine"},
		{Name: "web2", Image: "192.168.10.13:5000/nginx:1.25-alpine"},
	}
	if !reflect.DeepEqual(services, want) {
		t.Fatalf("services = %+v, want %+v", services, want)
	}
}

func TestT1StackYAML(t *testing.T) {
	got := t1StackYAML([]t1Service{
		{Name: "web1", Image: "nginx:1.24-alpine"},
		{Name: "web2", Image: "nginx:1.25-alpine"},
	})
	want := "services:\n" +
		"  web1:\n    image: nginx:1.24-alpine\n    deploy:\n      replicas: 1\n" +
		"  web2:\n    image: nginx:1.25-alpine\n    deploy:\n      replicas: 1\n"
	if got != want {
		t.Fatalf("t1StackYAML =\n%s\nwant\n%s", got, want)
	}
}

// TestT1PlanHealthcheckMatchesEvalWorkloadProbeContract is a regression
// test: the eval-workload image's task never leaves Swarm's "starting"
// state (and therefore never converges) without a healthcheck Swarm can
// actually gate on, matching the image's own -healthcheck probe exactly.
func TestT1PlanHealthcheckMatchesEvalWorkloadProbeContract(t *testing.T) {
	services, _ := t1Plan(1, 1, nil, "192.168.10.13:5000", "eval-workload", []string{"v1", "v2"}, true)
	got := t1StackYAML(services)
	want := "services:\n" +
		"  web1:\n    image: 192.168.10.13:5000/eval-workload:v2\n    deploy:\n      replicas: 1\n" +
		"    healthcheck:\n" +
		"      test: [\"CMD\", \"/app\", \"-healthcheck\"]\n" +
		"      interval: 2s\n" +
		"      timeout: 2s\n" +
		"      retries: 30\n" +
		"      start_period: 0s\n"
	if got != want {
		t.Fatalf("t1StackYAML =\n%s\nwant\n%s", got, want)
	}
}

func TestT1Condition(t *testing.T) {
	cfg := T1Config{Scale: 10, Changes: 2, Label: "events=on"}
	if got, want := t1Condition(cfg), "scale=10;changes=2;events=on"; got != want {
		t.Fatalf("t1Condition = %q, want %q", got, want)
	}
}
