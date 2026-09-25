package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testdata(name string) string {
	return filepath.Join("testdata", name)
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(testdata("minimal.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := Config{
		Git: Git{
			URL:    "ssh://git@example.com/deploy/stacks.git",
			Branch: "main",
			Path:   ".",
		},
		PollInterval:    30 * time.Second,
		Events:          Events{Wake: true},
		Prune:           true,
		Gate:            Gate{Enabled: false, Mode: GateModePerService},
		Telemetry:       Telemetry{Out: "./events.jsonl"},
		ConvergeTimeout: 5 * time.Minute,
		StageTimeout:    30 * time.Second,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("defaults mismatch:\n got  %+v\n want %+v", cfg, want)
	}
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(testdata("valid.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := Config{
		Git: Git{
			URL:               "ssh://git@example.com/deploy/stacks.git",
			Branch:            "release",
			Path:              "stacks/",
			SSHKeyFile:        "/etc/swarmgate/key",
			InterpolationVars: []string{"IMAGE_TAG", "DEPLOY_REGION"},
		},
		PollInterval: time.Minute,
		Events:       Events{Wake: false},
		Prune:        false,
		Registry:     Registry{AuthFile: "/etc/swarmgate/docker-config.json"},
		Gate: Gate{
			Enabled:    true,
			Mode:       GateModeAbortCycle,
			PolicyFile: "/etc/swarmgate/policy.yaml",
		},
		VolumeBindRoots: []string{"/datavol/data", "/var/lib/swarmgate"},
		Telemetry:       Telemetry{Out: "/var/lib/swarmgate/events.jsonl"},
		Docker:          Docker{Host: "unix:///var/run/docker.sock"},
		ConvergeTimeout: 10 * time.Minute,
		StageTimeout:    30 * time.Second,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("config mismatch:\n got  %+v\n want %+v", cfg, want)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		wantErr []string
	}{
		{
			name:    "missing git url",
			file:    "missing_git_url.yaml",
			wantErr: []string{"git.url is required"},
		},
		{
			name:    "gate enabled without policy file",
			file:    "gate_missing_policy_file.yaml",
			wantErr: []string{"gate.policy_file is required"},
		},
		{
			name:    "invalid gate mode",
			file:    "gate_bad_mode.yaml",
			wantErr: []string{`gate.mode must be "per-service" or "abort-cycle"`},
		},
		{
			name:    "invalid poll interval",
			file:    "bad_poll_interval.yaml",
			wantErr: []string{`poll_interval: invalid duration "soonish"`},
		},
		{
			name:    "invalid converge timeout",
			file:    "bad_converge_timeout.yaml",
			wantErr: []string{`converge_timeout: invalid duration "5 minutes"`},
		},
		{
			// Regression test: time.NewTicker(cfg.PollInterval) panics
			// outright on a non-positive duration (internal/loop/loop.go),
			// crashing the daemon at startup on a config typo like this
			// one. Load must reject it as an ordinary validation error
			// before it ever reaches the loop.
			name:    "zero poll interval",
			file:    "zero_poll_interval.yaml",
			wantErr: []string{"poll_interval must be at least"},
		},
		{
			name: "all errors collected",
			file: "multiple_errors.yaml",
			wantErr: []string{
				"git.url is required",
				"gate.policy_file is required",
				"gate.mode must be",
				"poll_interval: invalid duration",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(testdata(tt.file))
			if err == nil {
				t.Fatal("Load succeeded, want validation error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
		})
	}
}

func TestLoadUnknownKeys(t *testing.T) {
	tests := []struct {
		name string
		file string
	}{
		{"unknown top-level key", "unknown_key.yaml"},
		{"unknown nested key", "unknown_nested_key.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(testdata(tt.file))
			if err == nil {
				t.Fatal("Load succeeded, want error for unknown key")
			}
			if !strings.Contains(err.Error(), "not found") {
				t.Errorf("error %q does not look like an unknown-field error", err)
			}
		})
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		check func(t *testing.T, cfg Config)
	}{
		{
			name: "strings win over file values",
			env: map[string]string{
				"SWARMGATE_GIT_URL":                "https://example.com/other.git",
				"SWARMGATE_GIT_BRANCH":             "hotfix",
				"SWARMGATE_GIT_PATH":               "overlays/",
				"SWARMGATE_GIT_SSH_KEY_FILE":       "/run/secrets/key",
				"SWARMGATE_GIT_INTERPOLATION_VARS": "ONE, TWO ,THREE",
				"SWARMGATE_REGISTRY_AUTH_FILE":     "/run/secrets/auth.json",
				"SWARMGATE_GATE_MODE":              "per-service",
				"SWARMGATE_GATE_POLICY_FILE":       "/run/secrets/policy.yaml",
				"SWARMGATE_TELEMETRY_OUT":          "/tmp/events.jsonl",
				"SWARMGATE_DOCKER_HOST":            "tcp://docker:2376",
			},
			check: func(t *testing.T, cfg Config) {
				want := Git{
					URL:               "https://example.com/other.git",
					Branch:            "hotfix",
					Path:              "overlays/",
					SSHKeyFile:        "/run/secrets/key",
					InterpolationVars: []string{"ONE", "TWO", "THREE"},
				}
				if !reflect.DeepEqual(cfg.Git, want) {
					t.Errorf("Git = %+v, want %+v", cfg.Git, want)
				}
				if cfg.Registry.AuthFile != "/run/secrets/auth.json" {
					t.Errorf("Registry.AuthFile = %q", cfg.Registry.AuthFile)
				}
				if cfg.Gate.Mode != GateModePerService || cfg.Gate.PolicyFile != "/run/secrets/policy.yaml" {
					t.Errorf("Gate = %+v", cfg.Gate)
				}
				if cfg.Telemetry.Out != "/tmp/events.jsonl" {
					t.Errorf("Telemetry.Out = %q", cfg.Telemetry.Out)
				}
				if cfg.Docker.Host != "tcp://docker:2376" {
					t.Errorf("Docker.Host = %q", cfg.Docker.Host)
				}
			},
		},
		{
			name: "durations win over file values",
			env: map[string]string{
				"SWARMGATE_POLL_INTERVAL":    "90s",
				"SWARMGATE_CONVERGE_TIMEOUT": "2m",
				"SWARMGATE_STAGE_TIMEOUT":    "15s",
			},
			check: func(t *testing.T, cfg Config) {
				if cfg.PollInterval != 90*time.Second {
					t.Errorf("PollInterval = %v, want 90s", cfg.PollInterval)
				}
				if cfg.ConvergeTimeout != 2*time.Minute {
					t.Errorf("ConvergeTimeout = %v, want 2m", cfg.ConvergeTimeout)
				}
				if cfg.StageTimeout != 15*time.Second {
					t.Errorf("StageTimeout = %v, want 15s", cfg.StageTimeout)
				}
			},
		},
		{
			name: "explicit file false flipped true by env",
			env: map[string]string{
				"SWARMGATE_EVENTS_WAKE": "true",
				"SWARMGATE_PRUNE":       "true",
			},
			check: func(t *testing.T, cfg Config) {
				if !cfg.Events.Wake {
					t.Error("Events.Wake = false, want env override true")
				}
				if !cfg.Prune {
					t.Error("Prune = false, want env override true")
				}
			},
		},
		{
			name: "gate toggled off by env",
			env:  map[string]string{"SWARMGATE_GATE_ENABLED": "false"},
			check: func(t *testing.T, cfg Config) {
				if cfg.Gate.Enabled {
					t.Error("Gate.Enabled = true, want env override false")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg, err := Load(testdata("valid.yaml"))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tt.check(t, cfg)
		})
	}
}

func TestLoadEnvErrors(t *testing.T) {
	t.Setenv("SWARMGATE_PRUNE", "definitely")
	t.Setenv("SWARMGATE_POLL_INTERVAL", "fast")

	_, err := Load(testdata("valid.yaml"))
	if err == nil {
		t.Fatal("Load succeeded, want env parse errors")
	}
	for _, want := range []string{
		`SWARMGATE_PRUNE: invalid bool "definitely"`,
		`poll_interval: invalid duration "fast"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestLoadFileErrors(t *testing.T) {
	if _, err := Load(testdata("does_not_exist.yaml")); err == nil {
		t.Error("Load succeeded on missing file, want error")
	}
}

func TestLoadExampleFile(t *testing.T) {
	// The shipped example must always load cleanly.
	cfg, err := Load(filepath.Join("..", "..", "swarmgate.example.yaml"))
	if err != nil {
		t.Fatalf("Load example: %v", err)
	}
	if cfg.Git.URL == "" {
		t.Error("example config has empty git.url")
	}
}

func TestValidateVolumeBindMounts(t *testing.T) {
	cfg := Config{Git: Git{URL: "https://example.test/stacks.git"}, PollInterval: time.Second, ConvergeTimeout: time.Second, StageTimeout: time.Second}
	cfg.VolumeBindRoots = []string{"/"}
	cfg.VolumeBindMounts = []VolumeBindMount{{Source: "/", ReadOnly: false}}
	errs := cfg.validate()
	joined := ""
	for _, err := range errs {
		joined += err.Error() + "\n"
	}
	for _, want := range []string{"volume_bind_roots must not contain /", "must set read_only: true"} {
		if !strings.Contains(joined, want) {
			t.Errorf("validate errors = %q, want %q", joined, want)
		}
	}
}
