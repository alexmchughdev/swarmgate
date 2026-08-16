package spec

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexmchughdev/swarmgate/internal/source"
)

func loadFixture(t *testing.T, stack string) source.StackFile {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", stack+".yaml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return source.StackFile{Name: stack, Content: content}
}

func TestParseNormalForm(t *testing.T) {
	got, err := Parse([]source.StackFile{
		loadFixture(t, "alpha"),
		loadFixture(t, "beta"),
	}, nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := DesiredState{Services: map[string]ServiceSpec{
		"alpha_web": {
			Name:     "alpha_web",
			Image:    "nginx:1.27",
			Replicas: 3,
			Env: map[string]string{
				"MODE":                 "prod",
				"SWARMGATE_TEST_UNSET": "",
			},
			Labels: map[string]string{
				"swarmgate.tier":    "frontend",
				"swarmgate.managed": "true",
				"swarmgate.stack":   "alpha",
			},
			Networks: []string{"alpha_front"},
			Ports: []PortSpec{
				{Target: 80, Published: 8080, Protocol: "udp"},
				{Target: 443, Published: 8443, Protocol: "tcp"},
			},
			Healthcheck: &HealthcheckSpec{
				Test:        []string{"CMD", "curl", "-f", "http://localhost/"},
				Interval:    10 * time.Second,
				Timeout:     5 * time.Second,
				Retries:     3,
				StartPeriod: 30 * time.Second,
			},
		},
		"alpha_worker": {
			Name:     "alpha_worker",
			Image:    "alpine:3.20",
			Replicas: 1,
			Env:      map[string]string{"QUEUE": "jobs"},
			Labels: map[string]string{
				"swarmgate.managed": "true",
				"swarmgate.stack":   "alpha",
			},
			Networks: []string{"alpha_default"},
		},
		"beta_db": {
			Name:     "beta_db",
			Image:    "postgres:16",
			Replicas: 0,
			Env:      nil,
			Labels: map[string]string{
				"swarmgate.managed": "true",
				"swarmgate.stack":   "beta",
			},
			Networks: []string{"beta_default"},
		},
	}}

	if len(got.Services) != len(want.Services) {
		t.Fatalf("got %d services, want %d: %v", len(got.Services), len(want.Services), got.Services)
	}
	for name, w := range want.Services {
		g, ok := got.Services[name]
		if !ok {
			t.Errorf("missing service %q", name)
			continue
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("service %q:\n got  %+v\n want %+v", name, g, w)
		}
	}
}

// TestParseRejectsCrossStackQualifiedNameCollision is a regression test:
// ServiceName joins stack and service with an unescaped "_", so two
// different stacks can produce the same qualified name (stack "payments"
// service "db_writer" and stack "payments_db" service "writer" both
// qualify to "payments_db_writer"). Without a check, whichever file is
// parsed second would silently overwrite the first's service in the
// shared map — a real privilege-escalation path if the two stack files
// are owned by different, differently-trusted parties.
func TestParseRejectsCrossStackQualifiedNameCollision(t *testing.T) {
	payments := source.StackFile{Name: "payments", Content: []byte(`
services:
  db_writer:
    image: postgres:16
`)}
	paymentsDB := source.StackFile{Name: "payments_db", Content: []byte(`
services:
  writer:
    image: attacker/malicious:latest
`)}

	_, err := Parse([]source.StackFile{payments, paymentsDB}, nil)
	if err == nil {
		t.Fatal("Parse: expected a collision error, got nil")
	}
	if !strings.Contains(err.Error(), "payments_db_writer") {
		t.Errorf("error = %v, want it to name the colliding qualified service payments_db_writer", err)
	}
}

// TestParseRejectsTestlessHealthcheck is a regression test: a test-less
// healthcheck reads back as no healthcheck on the observed side, causing
// a permanent phantom diff. Parse rejects it instead.
func TestParseRejectsTestlessHealthcheck(t *testing.T) {
	f := source.StackFile{Name: "hc", Content: []byte(`
services:
  web:
    image: nginx:1.27
    healthcheck:
      interval: 30s
`)}
	_, err := Parse([]source.StackFile{f}, nil)
	if err == nil {
		t.Fatal("Parse: expected an error for a test-less healthcheck, got nil")
	}
	if !strings.Contains(err.Error(), "healthcheck with no test") {
		t.Errorf("error = %v, want it to explain the missing test", err)
	}
}

// TestParseRejectsUnsupportedHealthcheckSubkey is a regression test:
// start_interval parsed successfully and was silently dropped rather than
// deployed, and the reverse case (an operator setting it via the CLI) was
// invisible to the differ and wiped on the next unrelated update.
func TestParseRejectsUnsupportedHealthcheckSubkey(t *testing.T) {
	f := source.StackFile{Name: "hc", Content: []byte(`
services:
  web:
    image: nginx:1.27
    healthcheck:
      test: ["CMD", "true"]
      start_interval: 5s
`)}
	_, err := Parse([]source.StackFile{f}, nil)
	if err == nil {
		t.Fatal("Parse: expected an error for healthcheck.start_interval, got nil")
	}
	if !strings.Contains(err.Error(), `unsupported compose field "healthcheck.start_interval"`) {
		t.Errorf("error = %v, want it to name healthcheck.start_interval", err)
	}
}

// TestParseRejectsUnsupportedPortSubkey is a regression test: mode: host
// and host_ip parsed successfully and were silently dropped, deploying
// ingress publication instead of the host-mode publication requested.
func TestParseRejectsUnsupportedPortSubkey(t *testing.T) {
	f := source.StackFile{Name: "port", Content: []byte(`
services:
  web:
    image: nginx:1.27
    ports:
      - target: 80
        published: "8080"
        mode: host
`)}
	_, err := Parse([]source.StackFile{f}, nil)
	if err == nil {
		t.Fatal("Parse: expected an error for ports.mode, got nil")
	}
	if !strings.Contains(err.Error(), `unsupported compose field "ports.mode"`) {
		t.Errorf("error = %v, want it to name ports.mode", err)
	}
}

// TestParseAcceptsShortSyntaxPorts is a regression test: the ports subkey
// check must skip short-syntax entries (plain strings), which have no
// subkeys to validate.
func TestParseAcceptsShortSyntaxPorts(t *testing.T) {
	f := source.StackFile{Name: "port", Content: []byte(`
services:
  web:
    image: nginx:1.27
    ports:
      - "8080:80"
`)}
	if _, err := Parse([]source.StackFile{f}, nil); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

// TestParseRejectsExcessiveReplicas is a regression test for resource
// exhaustion: nothing about the compose spec itself bounds deploy.replicas,
// so whoever can push a stack file could otherwise ask the Docker API to
// scale a service to an arbitrary number of tasks on the next reconcile
// cycle.
func TestParseRejectsExcessiveReplicas(t *testing.T) {
	f := source.StackFile{Name: "huge", Content: []byte(`
services:
  web:
    image: nginx:1.27
    deploy:
      replicas: 100000
`)}
	_, err := Parse([]source.StackFile{f}, nil)
	if err == nil {
		t.Fatal("Parse: expected a replica-limit error, got nil")
	}
	if !strings.Contains(err.Error(), "replicas") || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want it to mention the replica limit", err)
	}
}

func TestParseInterpolation(t *testing.T) {
	t.Setenv("IMAGE_TAG", "v1.2.3")

	got, err := Parse([]source.StackFile{loadFixture(t, "interp")}, []string{"IMAGE_TAG"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	svc, ok := got.Services["interp_app"]
	if !ok {
		t.Fatalf("missing service interp_app: %v", got.Services)
	}
	if want := "registry.example.com/app:v1.2.3"; svc.Image != want {
		t.Errorf("image = %q, want %q", svc.Image, want)
	}
}

// TestParseInterpolationOnlyExposesAllowlistedVars is the security
// regression test for the fix above: a variable set in swarmgate's real
// process environment but NOT named in the allowlist must not be readable
// by stack-file interpolation, even though it's genuinely present in
// os.Environ(). Before this fix, Parse passed the full process
// environment to compose-go's interpolator regardless of what (if
// anything) the operator intended to expose.
func TestParseInterpolationOnlyExposesAllowlistedVars(t *testing.T) {
	t.Setenv("IMAGE_TAG", "v1.2.3")
	t.Setenv("SECRET_NOT_ALLOWLISTED", "top-secret-value")

	got, err := Parse([]source.StackFile{loadFixture(t, "interp")}, []string{"IMAGE_TAG"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	svc := got.Services["interp_app"]
	if strings.Contains(svc.Image, "top-secret-value") {
		t.Fatalf("image = %q leaked a non-allowlisted environment variable", svc.Image)
	}

	// With no allowlist at all, even the previously-working var must not
	// resolve — the default is zero exposure, not "whatever a caller
	// forgets to restrict."
	got, err = Parse([]source.StackFile{loadFixture(t, "interp")}, nil)
	if err != nil {
		t.Fatalf("Parse with nil allowlist: %v", err)
	}
	svc = got.Services["interp_app"]
	if strings.Contains(svc.Image, "v1.2.3") {
		t.Fatalf("image = %q resolved IMAGE_TAG despite no allowlist", svc.Image)
	}
}

func TestParseUnsupportedFields(t *testing.T) {
	_, err := Parse([]source.StackFile{
		loadFixture(t, "unsupported_service"),
		loadFixture(t, "unsupported_toplevel"),
	}, nil)
	if err == nil {
		t.Fatal("Parse: expected error, got nil")
	}

	// All offenders across both files must be reported in one error.
	for _, want := range []string{
		`unsupported compose field "volumes" in service "unsupported_service_web"`,
		`unsupported compose field "depends_on" in service "unsupported_service_web"`,
		`unsupported compose field "deploy.resources" in service "unsupported_service_web"`,
		`unsupported compose field "volumes" in stack "unsupported_toplevel"`,
		`unsupported compose field "secrets" in stack "unsupported_toplevel"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q\nfull error:\n%v", want, err)
		}
	}
}
