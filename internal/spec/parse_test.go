package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"

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

func TestParseEnvFileHostRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.env"), []byte("FROM_FILE=resolved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := source.StackFile{Name: "app", Content: []byte("services:\n  web:\n    image: nginx:1.27\n    env_file: app.env\n")}
	t.Run("valid resolution", func(t *testing.T) {
		got, err := Parse([]source.StackFile{f}, nil, root)
		if err != nil {
			t.Fatal(err)
		}
		if got.Services["app_web"].Env["FROM_FILE"] != "resolved" {
			t.Fatalf("Env = %#v", got.Services["app_web"].Env)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		missing := source.StackFile{Name: "app", Content: []byte("services:\n  web:\n    image: nginx:1.27\n    env_file: missing.env\n")}
		if _, err := Parse([]source.StackFile{missing}, nil, root); err == nil || !strings.Contains(err.Error(), "env_file") {
			t.Fatalf("Parse error = %v, want missing env_file", err)
		}
	})
	t.Run("path escape", func(t *testing.T) {
		outside := filepath.Join(filepath.Dir(root), "outside.env")
		if err := os.WriteFile(outside, []byte("OUTSIDE=yes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		escape := source.StackFile{Name: "app", Content: []byte("services:\n  web:\n    image: nginx:1.27\n    env_file: ../outside.env\n")}
		if _, err := Parse([]source.StackFile{escape}, nil, root); err == nil || !strings.Contains(err.Error(), "escapes env_file_root") {
			t.Fatalf("Parse error = %v, want path escape", err)
		}
	})
	t.Run("unconfigured root", func(t *testing.T) {
		if _, err := Parse([]source.StackFile{f}, nil); err == nil || !strings.Contains(err.Error(), "env_file_root is configured") {
			t.Fatalf("Parse error = %v, want clear configuration rejection", err)
		}
	})
}

func TestParseGitDefinedConfig(t *testing.T) {
	f := source.StackFile{Name: "monitoring", Path: "stacks/00-monitoring.yml", Content: []byte(`services:
  web:
    image: nginx:1.27
    configs:
      - source: nginx_conf
        target: /etc/nginx/entrypoint.conf
        mode: 0555
configs:
  nginx_conf:
    name: nginx_conf_v4
    file: ./swarm-config/nginx.conf
`), Files: map[string][]byte{"stacks/swarm-config/nginx.conf": []byte("new config")}}
	got, err := Parse([]source.StackFile{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Configs["nginx_conf_v4"].Data) != "new config" {
		t.Fatalf("config payload = %+v", got.Configs)
	}
	refs := got.Services["monitoring_web"].Configs
	if len(refs) != 1 || refs[0].Source != "nginx_conf_v4" || refs[0].Mode != 0555 {
		t.Fatalf("config refs = %+v", refs)
	}
}

func TestParseEnvFileAbsolutePathsStayWithinRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "app.env")
	if err := os.WriteFile(inside, []byte("FROM_FILE=resolved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked.env")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}
	parsePath := func(path, envRoot string) error {
		f := source.StackFile{Name: "app", Content: []byte(fmt.Sprintf("services:\n  web:\n    image: nginx:1.27\n    env_file: %q\n", path))}
		_, err := Parse([]source.StackFile{f}, nil, envRoot)
		return err
	}
	for _, p := range []string{inside, link} {
		if err := parsePath(p, root); err != nil {
			t.Errorf("Parse absolute env_file %q: %v", p, err)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside.env")
	if err := os.WriteFile(outside, []byte("SECRET=outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := parsePath(outside, root); err == nil || !strings.Contains(err.Error(), "escapes env_file_root") {
		t.Fatalf("Parse outside absolute env_file error = %v, want root escape", err)
	}
	if err := parsePath(inside, ""); err == nil || !strings.Contains(err.Error(), "env_file_root is configured") {
		t.Fatalf("Parse without env_file_root error = %v, want configuration rejection", err)
	}
}

func TestParseBindMountVolumeAllowedRoots(t *testing.T) {
	root := t.TempDir()
	otherRoot := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "data-link")
	if err := os.Symlink(data, link); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(otherRoot, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	parse := func(sourcePath string, roots []string) (DesiredState, error) {
		f := source.StackFile{Name: "app", Content: []byte(fmt.Sprintf("services:\n  web:\n    image: nginx:1.27\n    volumes:\n      - type: bind\n        source: %q\n        target: /srv/data\n", sourcePath))}
		return ParseWithRoots([]source.StackFile{f}, nil, "", roots)
	}
	got, err := parse(link, []string{otherRoot, root})
	if err != nil {
		t.Fatalf("Parse allowed bind mount: %v", err)
	}
	if got.Services["app_web"].Volumes[0].Source != data {
		t.Errorf("bind source = %q, want resolved path %q", got.Services["app_web"].Volumes[0].Source, data)
	}
	if _, err := parse(data, nil); err == nil || !strings.Contains(err.Error(), "volume_bind_roots") {
		t.Errorf("Parse with no roots error = %v, want explicit opt-in rejection", err)
	}
	if _, err := parse(outside, []string{root}); err == nil || !strings.Contains(err.Error(), "outside configured volume_bind_roots") {
		t.Errorf("Parse outside allowed roots error = %v, want rejection", err)
	}
	if _, err := parse("./relative-data", []string{root}); err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
		t.Errorf("Parse relative bind source error = %v, want explicit relative-path rejection", err)
	}
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
				{Target: 80, Published: 8080, Protocol: "udp", Mode: "ingress"},
				{Target: 443, Published: 8443, Protocol: "tcp", Mode: "ingress"},
			},
			Healthcheck: &HealthcheckSpec{
				Test:        []string{"CMD", "curl", "-f", "http://localhost/"},
				Interval:    10 * time.Second,
				Timeout:     5 * time.Second,
				Retries:     3,
				StartPeriod: 30 * time.Second,
			},
			Mode: "replicated",
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
			Mode:     "replicated",
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
			Mode:     "replicated",
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

// TestParseRejectsUnsupportedPortSubkey is a regression test: an
// unmodelled long-syntax port field must still fail the parse rather than
// be silently dropped.
func TestParseRejectsUnsupportedPortSubkey(t *testing.T) {
	f := source.StackFile{Name: "port", Content: []byte(`
services:
  web:
    image: nginx:1.27
    ports:
      - target: 80
        published: "8080"
        host_ip: 127.0.0.1
`)}
	_, err := Parse([]source.StackFile{f}, nil)
	if err == nil {
		t.Fatal("Parse: expected an error for ports.host_ip, got nil")
	}
	if !strings.Contains(err.Error(), `unsupported compose field "ports.host_ip"`) {
		t.Errorf("error = %v, want it to name ports.host_ip", err)
	}
}

// TestParsePortModeHostRoundTrips is a regression test: mode: host used to
// parse successfully and then be silently dropped, deploying ingress
// publication instead of the host-mode publication requested. It must now
// survive parse, Normalize, and the ToSwarm/FromSwarm round trip unchanged.
func TestParsePortModeHostRoundTrips(t *testing.T) {
	f := source.StackFile{Name: "port", Content: []byte(`
services:
  web:
    image: nginx:1.27
    ports:
      - target: 80
        published: 8080
        mode: host
`)}
	got, err := Parse([]source.StackFile{f}, nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	svc := got.Services["port_web"]
	want := []PortSpec{{Target: 80, Published: 8080, Protocol: "tcp", Mode: "host"}}
	if !reflect.DeepEqual(svc.Ports, want) {
		t.Fatalf("Ports = %+v, want %+v", svc.Ports, want)
	}
	roundTripped := FromSwarm(swarmService(t, svc), nil)
	if roundTripped.Ports[0].Mode != "host" {
		t.Fatalf("port mode after ToSwarm/FromSwarm round trip = %q, want %q (must not silently become ingress)", roundTripped.Ports[0].Mode, "host")
	}
}

// swarmService renders a normal-form spec into a swarm.Service wrapper, the
// shape FromSwarm consumes, for round-trip assertions.
func swarmService(t *testing.T, s ServiceSpec) swarm.Service {
	t.Helper()
	sw := ToSwarm(s)
	return swarm.Service{Spec: sw}
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

// TestParseNewComposeFields is table-driven: one accept and one reject case
// (at minimum) per field added alongside the resource, config/secret,
// container-option, and deploy-option support in this file. Each accept
// case's check function asserts the specific normal-form value the field
// produced, not just that Parse succeeded.
func TestParseNewComposeFields(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string // substring; empty means Parse must succeed
		check   func(t *testing.T, svc ServiceSpec)
	}{
		{
			name: "volumes: named local-driver volume accepted",
			yaml: `
services:
  web:
    image: nginx:1.27
    volumes:
      - data:/var/lib/data
volumes:
  data: {}
`,
			check: func(t *testing.T, svc ServiceSpec) {
				want := []VolumeMount{{Source: "data", Target: "/var/lib/data"}}
				if !reflect.DeepEqual(svc.Volumes, want) {
					t.Errorf("Volumes = %+v, want %+v", svc.Volumes, want)
				}
			},
		},
		{
			name: "volumes: bind mount rejected",
			yaml: `
services:
  web:
    image: nginx:1.27
    volumes:
      - /host/data:/var/lib/data
`,
			wantErr: "configure volume_bind_roots",
		},
		{
			name: "volumes: top-level declaration with a driver rejected",
			yaml: `
services:
  web:
    image: nginx:1.27
    volumes:
      - data:/var/lib/data
volumes:
  data:
    driver: nfs
`,
			wantErr: `unsupported compose field "volumes.data"`,
		},
		{
			name: "env_file rejected when env_file_root is unset",
			yaml: `
services:
  web:
    image: nginx:1.27
    env_file:
      - .env
`,
			wantErr: "env_file_root is configured",
		},
		{
			name: "configs: external reference accepted",
			yaml: `
services:
  web:
    image: nginx:1.27
    configs:
      - source: myconf
        target: /etc/myconf
configs:
  myconf:
    external: true
`,
			check: func(t *testing.T, svc ServiceSpec) {
				want := []FileRef{{Source: "myconf", Target: "/etc/myconf"}}
				if !reflect.DeepEqual(svc.Configs, want) {
					t.Errorf("Configs = %+v, want %+v", svc.Configs, want)
				}
			},
		},
		{
			name: "configs: file content accepted",
			yaml: `
services:
  web:
    image: nginx:1.27
    configs:
      - source: myconf
        target: /etc/myconf
configs:
  myconf:
    file: ./myconf.txt
`,
			check: func(t *testing.T, svc ServiceSpec) {
				if !reflect.DeepEqual(svc.Configs, []FileRef{{Source: "myconf", Target: "/etc/myconf"}}) {
					t.Fatalf("Configs = %+v", svc.Configs)
				}
			},
		},
		{
			name: "secrets: external reference accepted",
			yaml: `
services:
  web:
    image: nginx:1.27
    secrets:
      - source: mysecret
        target: /run/secrets/mysecret
secrets:
  mysecret:
    external: true
`,
			check: func(t *testing.T, svc ServiceSpec) {
				want := []FileRef{{Source: "mysecret", Target: "/run/secrets/mysecret"}}
				if !reflect.DeepEqual(svc.Secrets, want) {
					t.Errorf("Secrets = %+v, want %+v", svc.Secrets, want)
				}
			},
		},
		{
			name: "secrets: inline content rejected",
			yaml: `
services:
  web:
    image: nginx:1.27
    secrets:
      - source: mysecret
        target: /run/secrets/mysecret
secrets:
  mysecret:
    content: hunter2
`,
			wantErr: `secrets "mysecret" must be external only`,
		},
		{
			// compose-go itself refuses a reference to an undeclared name
			// before swarmgate's own external-only check ever runs;
			// serviceFileRefs' declared-name check exists as a second
			// layer in case that upstream validation ever changes.
			name: "secrets: reference to an undeclared name rejected",
			yaml: `
services:
  web:
    image: nginx:1.27
    secrets:
      - source: mysecret
`,
			wantErr: "undefined secret mysecret",
		},
		{
			name: "command, entrypoint, hostname, user, cap_add, stop_grace_period accepted",
			yaml: `
services:
  web:
    image: nginx:1.27
    command: ["nginx", "-g", "daemon off;"]
    entrypoint: ["/entry.sh"]
    hostname: "{{.Node.Hostname}}"
    user: "1000:1000"
    cap_add: ["NET_ADMIN", "SYS_TIME"]
    stop_grace_period: 15s
`,
			check: func(t *testing.T, svc ServiceSpec) {
				if !reflect.DeepEqual(svc.Command, []string{"nginx", "-g", "daemon off;"}) {
					t.Errorf("Command = %v", svc.Command)
				}
				if !reflect.DeepEqual(svc.Entrypoint, []string{"/entry.sh"}) {
					t.Errorf("Entrypoint = %v", svc.Entrypoint)
				}
				if svc.Hostname != "{{.Node.Hostname}}" {
					t.Errorf("Hostname = %q, want the Swarm placeholder passed through unresolved", svc.Hostname)
				}
				if svc.User != "1000:1000" {
					t.Errorf("User = %q", svc.User)
				}
				if !reflect.DeepEqual(svc.CapAdd, []string{"NET_ADMIN", "SYS_TIME"}) {
					t.Errorf("CapAdd = %v", svc.CapAdd)
				}
				if svc.StopGracePeriod != 15*time.Second {
					t.Errorf("StopGracePeriod = %v, want 15s", svc.StopGracePeriod)
				}
			},
		},
		{
			name:    "cap_add: exceeding the resource cap rejected",
			yaml:    "services:\n  web:\n    image: nginx:1.27\n    cap_add: [" + distinctCapNames(maxCapAdd+1) + "]\n",
			wantErr: "exceeds the 50 limit",
		},
		{
			name: "ulimits: shorthand and long form accepted",
			yaml: `
services:
  web:
    image: nginx:1.27
    ulimits:
      nproc: 65535
      nofile:
        soft: 1024
        hard: 2048
`,
			check: func(t *testing.T, svc ServiceSpec) {
				want := []UlimitSpec{{Name: "nofile", Soft: 1024, Hard: 2048}, {Name: "nproc", Soft: 65535, Hard: 65535}}
				if !reflect.DeepEqual(svc.Ulimits, want) {
					t.Errorf("Ulimits = %+v, want %+v", svc.Ulimits, want)
				}
			},
		},
		{
			name: "deploy.mode: global accepted",
			yaml: `
services:
  web:
    image: nginx:1.27
    deploy:
      mode: global
`,
			check: func(t *testing.T, svc ServiceSpec) {
				if svc.Mode != "global" {
					t.Errorf("Mode = %q, want global", svc.Mode)
				}
			},
		},
		{
			name: "deploy.mode: invalid value rejected",
			yaml: `
services:
  web:
    image: nginx:1.27
    deploy:
      mode: sometimes
`,
			wantErr: `deploy.mode "sometimes" is not one of`,
		},
		{
			name: "deploy.restart_policy accepted",
			yaml: `
services:
  web:
    image: nginx:1.27
    deploy:
      restart_policy:
        condition: on-failure
        delay: 5s
        max_attempts: 3
        window: 30s
`,
			check: func(t *testing.T, svc ServiceSpec) {
				want := &RestartPolicySpec{Condition: "on-failure", Delay: 5 * time.Second, MaxAttempts: 3, Window: 30 * time.Second}
				if !reflect.DeepEqual(svc.RestartPolicy, want) {
					t.Errorf("RestartPolicy = %+v, want %+v", svc.RestartPolicy, want)
				}
			},
		},
		{
			name: "deploy.resources: memory and cpu limits accepted",
			yaml: `
services:
  web:
    image: nginx:1.27
    deploy:
      resources:
        limits:
          memory: 128M
          cpus: "0.5"
`,
			check: func(t *testing.T, svc ServiceSpec) {
				want := &ResourcesSpec{MemoryBytes: 128 * 1024 * 1024, NanoCPUs: 500000000}
				if !reflect.DeepEqual(svc.Resources, want) {
					t.Errorf("Resources = %+v, want %+v", svc.Resources, want)
				}
			},
		},
		{
			name: "deploy.resources: limits without memory rejected",
			yaml: `
services:
  web:
    image: nginx:1.27
    deploy:
      resources:
        limits:
          cpus: "0.5"
`,
			wantErr: "resources.limits.memory is required",
		},
		{
			name: "deploy.placement: constraints accepted",
			yaml: `
services:
  web:
    image: nginx:1.27
    deploy:
      placement:
        constraints:
          - node.role==worker
          - node.labels.zone==east
`,
			check: func(t *testing.T, svc ServiceSpec) {
				want := &PlacementSpec{Constraints: []string{"node.labels.zone==east", "node.role==worker"}}
				if !reflect.DeepEqual(svc.Placement, want) {
					t.Errorf("Placement = %+v, want %+v", svc.Placement, want)
				}
			},
		},
		{
			name: "deploy.update_config: explicit positive parallelism accepted",
			yaml: `
services:
  web:
    image: nginx:1.27
    deploy:
      update_config:
        parallelism: 2
`,
			check: func(t *testing.T, svc ServiceSpec) {
				want := &UpdateConfigSpec{Parallelism: 2}
				if !reflect.DeepEqual(svc.UpdateConfig, want) {
					t.Errorf("UpdateConfig = %+v, want %+v", svc.UpdateConfig, want)
				}
			},
		},
		{
			name: "deploy.update_config: zero parallelism rejected",
			yaml: `
services:
  web:
    image: nginx:1.27
    deploy:
      update_config:
        parallelism: 0
`,
			wantErr: "update_config.parallelism must be set and greater than zero",
		},
		{
			name: "x- extension anchor referenced by alias resolves before validation",
			yaml: `
x-restart-policy: &restart-policy
  condition: on-failure
  max_attempts: 5
services:
  web:
    image: nginx:1.27
    deploy:
      restart_policy: *restart-policy
`,
			check: func(t *testing.T, svc ServiceSpec) {
				want := &RestartPolicySpec{Condition: "on-failure", MaxAttempts: 5}
				if !reflect.DeepEqual(svc.RestartPolicy, want) {
					t.Errorf("RestartPolicy = %+v, want %+v", svc.RestartPolicy, want)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := source.StackFile{Name: "fields", Content: []byte(tt.yaml), Files: map[string][]byte{"myconf.txt": []byte("config contents")}}
			got, err := Parse([]source.StackFile{f}, nil)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Parse: expected an error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			svc, ok := got.Services["fields_web"]
			if !ok {
				t.Fatalf("missing service fields_web: %v", got.Services)
			}
			if tt.check != nil {
				tt.check(t, svc)
			}
		})
	}
}

// distinctCapNames returns n distinct, quoted, comma-separated fake
// capability names, so a resource-cap test isn't accidentally defeated by
// Normalize's cap_add deduplication collapsing repeated identical values.
func distinctCapNames(n int) string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("%q", fmt.Sprintf("CAP_FAKE_%d", i))
	}
	return strings.Join(names, ", ")
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
		`unsupported compose field "working_dir" in service "unsupported_service_web"`,
		`unsupported compose field "depends_on" in service "unsupported_service_web"`,
		`unsupported compose field "deploy.rollback_config" in service "unsupported_service_web"`,
		`unsupported compose field "volumes.data" in stack "unsupported_toplevel"`,
		`unsupported compose field "secrets.file" in stack "unsupported_toplevel"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q\nfull error:\n%v", want, err)
		}
	}
}
