package harness

import (
	"reflect"
	"testing"
)

func TestScalePlanFreshStackStartsAtFirstTag(t *testing.T) {
	services, next := scalePlan(3, 1, nil, "", "", nil, false)
	want := []scaleService{
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

func TestScalePlanBumpsOnlyChangesServices(t *testing.T) {
	services, next := scalePlan(5, 2, []int{0, 0, 0, 0, 0}, "", "", nil, false)
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

func TestScalePlanCyclesAndWrapsAroundPinnedTags(t *testing.T) {
	tagIndex := []int{0}
	var seen []string
	for i := 0; i < len(scaleTags)+2; i++ {
		services, next := scalePlan(1, 1, tagIndex, "", "", nil, false)
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

func TestScalePlanChangesGreaterThanScaleClampsToScale(t *testing.T) {
	services, next := scalePlan(2, 10, []int{0, 0}, "", "", nil, false)
	for i, s := range services {
		if s.Image != "nginx:1.25-alpine" {
			t.Fatalf("service %d = %+v, want all bumped exactly once despite changes > scale", i, s)
		}
	}
	if !reflect.DeepEqual(next, []int{1, 1}) {
		t.Fatalf("next tag index = %v, want [1 1]", next)
	}
}

func TestScalePlanMismatchedTagIndexTreatedAsFresh(t *testing.T) {
	// A tagIndex sized for a different scale (e.g. a stack that just grew)
	// must not panic or misalign; it resets to a fresh stack instead.
	services, next := scalePlan(3, 1, []int{4, 4}, "", "", nil, false)
	if len(services) != 3 || len(next) != 3 {
		t.Fatalf("services=%v next=%v, want length 3 each", services, next)
	}
	if services[0].Image != "nginx:1.25-alpine" {
		t.Fatalf("service[0] = %+v, want fresh-stack bump", services[0])
	}
}

func TestScalePlanDeterministic(t *testing.T) {
	a, nextA := scalePlan(10, 3, []int{2, 2, 2, 2, 2, 2, 2, 2, 2, 2}, "", "", nil, false)
	b, nextB := scalePlan(10, 3, []int{2, 2, 2, 2, 2, 2, 2, 2, 2, 2}, "", "", nil, false)
	if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(nextA, nextB) {
		t.Fatalf("scalePlan not deterministic for identical input")
	}
}

// TestScalePlanRegistryPrefixesImage is a regression test: a bare nginx:tag
// reference implies Docker Hub to both the engine and swarmgate's own
// resolve stage, so every resolve during a run would be a real WAN
// round trip unless the harness can route through a local mirror instead.
func TestScalePlanRegistryPrefixesImage(t *testing.T) {
	services, _ := scalePlan(2, 2, nil, "192.168.10.13:5000", "", nil, false)
	want := []scaleService{
		{Name: "web1", Image: "192.168.10.13:5000/nginx:1.25-alpine"},
		{Name: "web2", Image: "192.168.10.13:5000/nginx:1.25-alpine"},
	}
	if !reflect.DeepEqual(services, want) {
		t.Fatalf("services = %+v, want %+v", services, want)
	}
}

func TestScaleStackYAML(t *testing.T) {
	got := scaleStackYAML([]scaleService{
		{Name: "web1", Image: "nginx:1.24-alpine"},
		{Name: "web2", Image: "nginx:1.25-alpine"},
	})
	want := "services:\n" +
		"  web1:\n    image: nginx:1.24-alpine\n    deploy:\n      replicas: 1\n" +
		"  web2:\n    image: nginx:1.25-alpine\n    deploy:\n      replicas: 1\n"
	if got != want {
		t.Fatalf("scaleStackYAML =\n%s\nwant\n%s", got, want)
	}
}

// TestScalePlanHealthcheckMatchesEvalWorkloadProbeContract is a regression
// test: the eval-workload image's task never leaves Swarm's "starting"
// state (and therefore never converges) without a healthcheck Swarm can
// actually gate on, matching the image's own -healthcheck probe exactly.
func TestScalePlanHealthcheckMatchesEvalWorkloadProbeContract(t *testing.T) {
	services, _ := scalePlan(1, 1, nil, "192.168.10.13:5000", "eval-workload", []string{"v1", "v2"}, true)
	got := scaleStackYAML(services)
	want := "services:\n" +
		"  web1:\n    image: 192.168.10.13:5000/eval-workload:v2\n    deploy:\n      replicas: 1\n" +
		"    healthcheck:\n" +
		"      test: [\"CMD\", \"/app\", \"-healthcheck\"]\n" +
		"      interval: 2s\n" +
		"      timeout: 2s\n" +
		"      retries: 30\n" +
		"      start_period: 0s\n"
	if got != want {
		t.Fatalf("scaleStackYAML =\n%s\nwant\n%s", got, want)
	}
}

func TestScaleCondition(t *testing.T) {
	cfg := ScaleConfig{Scale: 10, Changes: 2, Label: "events=on"}
	if got, want := scaleCondition(cfg), "scale=10;changes=2;events=on"; got != want {
		t.Fatalf("scaleCondition = %q, want %q", got, want)
	}
}
