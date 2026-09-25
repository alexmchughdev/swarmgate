package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Gate modes control how a denied policy decision affects a converge cycle.
const (
	GateModePerService = "per-service"
	GateModeAbortCycle = "abort-cycle"
)

// Config is the fully resolved swarmgate configuration: file values with
// environment overrides applied, defaults filled in, and validation passed.
type Config struct {
	Git              Git
	EnvFileRoot      string
	VolumeBindRoots  []string
	VolumeBindMounts []VolumeBindMount
	PollInterval     time.Duration
	Events           Events
	Prune            bool
	Registry         Registry
	Gate             Gate
	Telemetry        Telemetry
	Docker           Docker
	ConvergeTimeout  time.Duration
	// StageTimeout bounds each individual git/registry/Docker API call the
	// reconcile loop makes before apply (poll, resolve, observe) and each
	// individual apply call — everything except AwaitConverged, which
	// already has its own explicit ConvergeTimeout. Without this, a
	// remote that accepts a connection and never responds (a slow or
	// interfered-with git host, registry, or Docker socket) hangs the
	// whole cycle, and therefore every stack this instance manages,
	// indefinitely — the process's own lifetime context carries no
	// deadline of its own.
	StageTimeout time.Duration
}

// Git describes the repository that holds the stack definitions.
type Git struct {
	URL        string
	Branch     string
	Path       string
	SSHKeyFile string
	// InterpolationVars names the swarmgate process environment variables,
	// if any, that stack files may reference via compose's ${VAR}
	// interpolation. Anyone who can push a stack file can read back
	// whatever these resolve to (deployed into a live service spec, or
	// echoed into telemetry) — empty by default so stack files see no
	// part of swarmgate's own environment unless explicitly opted in.
	InterpolationVars []string
}

// VolumeBindMount authorizes one exact host bind source only when its read-only mode matches.
type VolumeBindMount struct {
	Source   string `yaml:"source"`
	ReadOnly bool   `yaml:"read_only"`
}

// Events controls reaction to Docker engine events.
type Events struct {
	Wake bool
}

// Registry holds registry authentication settings.
type Registry struct {
	AuthFile string
}

// Gate configures the deployment approval gate.
type Gate struct {
	Enabled    bool
	Mode       string
	PolicyFile string
}

// Telemetry configures event output.
type Telemetry struct {
	Out string
}

// Docker holds Docker engine connection settings.
type Docker struct {
	Host string
}

// rawConfig mirrors the YAML schema before defaulting. Bools that default to
// true are pointers so an explicit false in the file is distinguishable from
// absence; durations stay strings so file and env values share one parse path.
type rawConfig struct {
	Git struct {
		URL               string   `yaml:"url"`
		Branch            string   `yaml:"branch"`
		Path              string   `yaml:"path"`
		SSHKeyFile        string   `yaml:"ssh_key_file"`
		InterpolationVars []string `yaml:"interpolation_vars"`
	} `yaml:"git"`
	PollInterval string `yaml:"poll_interval"`
	Events       struct {
		Wake *bool `yaml:"wake"`
	} `yaml:"events"`
	Prune    *bool `yaml:"prune"`
	Registry struct {
		AuthFile string `yaml:"auth_file"`
	} `yaml:"registry"`
	Gate struct {
		Enabled    *bool  `yaml:"enabled"`
		Mode       string `yaml:"mode"`
		PolicyFile string `yaml:"policy_file"`
	} `yaml:"gate"`
	Telemetry struct {
		Out string `yaml:"out"`
	} `yaml:"telemetry"`
	Docker struct {
		Host string `yaml:"host"`
	} `yaml:"docker"`
	ConvergeTimeout  string            `yaml:"converge_timeout"`
	StageTimeout     string            `yaml:"stage_timeout"`
	EnvFileRoot      string            `yaml:"env_file_root"`
	VolumeBindRoots  []string          `yaml:"volume_bind_roots"`
	VolumeBindMounts []VolumeBindMount `yaml:"volume_bind_mounts"`
}

// envOverrides maps every SWARMGATE_* variable to its raw config field.
// Kept as an explicit table so the supported surface is greppable and there
// is no reflection to reason about.
var envOverrides = []struct {
	key   string
	apply func(r *rawConfig, v string) error
}{
	{"SWARMGATE_GIT_URL", func(r *rawConfig, v string) error { r.Git.URL = v; return nil }},
	{"SWARMGATE_GIT_BRANCH", func(r *rawConfig, v string) error { r.Git.Branch = v; return nil }},
	{"SWARMGATE_GIT_PATH", func(r *rawConfig, v string) error { r.Git.Path = v; return nil }},
	{"SWARMGATE_GIT_SSH_KEY_FILE", func(r *rawConfig, v string) error { r.Git.SSHKeyFile = v; return nil }},
	{"SWARMGATE_GIT_INTERPOLATION_VARS", func(r *rawConfig, v string) error { r.Git.InterpolationVars = splitCommaList(v); return nil }},
	{"SWARMGATE_ENV_FILE_ROOT", func(r *rawConfig, v string) error { r.EnvFileRoot = v; return nil }},
	{"SWARMGATE_VOLUME_BIND_ROOTS", func(r *rawConfig, v string) error { r.VolumeBindRoots = splitCommaList(v); return nil }},
	{"SWARMGATE_POLL_INTERVAL", func(r *rawConfig, v string) error { r.PollInterval = v; return nil }},
	{"SWARMGATE_EVENTS_WAKE", func(r *rawConfig, v string) error { return setBool(&r.Events.Wake, "SWARMGATE_EVENTS_WAKE", v) }},
	{"SWARMGATE_PRUNE", func(r *rawConfig, v string) error { return setBool(&r.Prune, "SWARMGATE_PRUNE", v) }},
	{"SWARMGATE_REGISTRY_AUTH_FILE", func(r *rawConfig, v string) error { r.Registry.AuthFile = v; return nil }},
	{"SWARMGATE_GATE_ENABLED", func(r *rawConfig, v string) error { return setBool(&r.Gate.Enabled, "SWARMGATE_GATE_ENABLED", v) }},
	{"SWARMGATE_GATE_MODE", func(r *rawConfig, v string) error { r.Gate.Mode = v; return nil }},
	{"SWARMGATE_GATE_POLICY_FILE", func(r *rawConfig, v string) error { r.Gate.PolicyFile = v; return nil }},
	{"SWARMGATE_TELEMETRY_OUT", func(r *rawConfig, v string) error { r.Telemetry.Out = v; return nil }},
	{"SWARMGATE_DOCKER_HOST", func(r *rawConfig, v string) error { r.Docker.Host = v; return nil }},
	{"SWARMGATE_CONVERGE_TIMEOUT", func(r *rawConfig, v string) error { r.ConvergeTimeout = v; return nil }},
	{"SWARMGATE_STAGE_TIMEOUT", func(r *rawConfig, v string) error { r.StageTimeout = v; return nil }},
}

func setBool(dst **bool, key, v string) error {
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("%s: invalid bool %q: %w", key, v, err)
	}
	*dst = &b
	return nil
}

// Load reads the YAML file at path, applies SWARMGATE_* environment
// overrides, fills defaults, and validates. All validation errors are
// collected and returned joined rather than failing on the first.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	// KnownFields rejects any key not in rawConfig's schema, at every
	// nesting level (e.g. a typo'd "git.uri" or a stray top-level key from
	// a copy-pasted example) -- a silently-ignored key is indistinguishable
	// from a config that "worked" until the missing setting matters at
	// runtime, which for something like gate.enabled is a security-relevant
	// silent failure, not just a typo.
	var raw rawConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}

	var errs []error

	// Env overrides are applied after file decode so they always win.
	for _, o := range envOverrides {
		if v, ok := os.LookupEnv(o.key); ok {
			if err := o.apply(&raw, v); err != nil {
				errs = append(errs, err)
			}
		}
	}

	cfg := Config{
		EnvFileRoot:      raw.EnvFileRoot,
		VolumeBindRoots:  raw.VolumeBindRoots,
		VolumeBindMounts: raw.VolumeBindMounts,
		Git: Git{
			URL:               raw.Git.URL,
			Branch:            defaultString(raw.Git.Branch, "main"),
			Path:              defaultString(raw.Git.Path, "."),
			SSHKeyFile:        raw.Git.SSHKeyFile,
			InterpolationVars: raw.Git.InterpolationVars,
		},
		Events: Events{Wake: defaultBool(raw.Events.Wake, true)},
		Prune:  defaultBool(raw.Prune, true),
		Registry: Registry{
			AuthFile: raw.Registry.AuthFile,
		},
		Gate: Gate{
			Enabled:    defaultBool(raw.Gate.Enabled, false),
			Mode:       defaultString(raw.Gate.Mode, GateModePerService),
			PolicyFile: raw.Gate.PolicyFile,
		},
		Telemetry: Telemetry{Out: defaultString(raw.Telemetry.Out, "./events.jsonl")},
		Docker:    Docker{Host: raw.Docker.Host},
	}

	cfg.PollInterval, err = parseDuration("poll_interval", raw.PollInterval, 30*time.Second)
	if err != nil {
		errs = append(errs, err)
	}
	cfg.ConvergeTimeout, err = parseDuration("converge_timeout", raw.ConvergeTimeout, 5*time.Minute)
	if err != nil {
		errs = append(errs, err)
	}
	cfg.StageTimeout, err = parseDuration("stage_timeout", raw.StageTimeout, 30*time.Second)
	if err != nil {
		errs = append(errs, err)
	}

	errs = append(errs, cfg.validate()...)

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// minDuration floors poll_interval/converge_timeout/stage_timeout: below
// this, poll_interval reaching time.NewTicker as 0 or negative panics
// outright (a config typo crashing the daemon at startup), and any of the
// three at an absurdly small but positive value turns into a tight loop
// hammering git, the registry, or the Docker API.
const minDuration = 100 * time.Millisecond

func (c Config) validate() []error {
	var errs []error
	if c.Git.URL == "" {
		errs = append(errs, errors.New("git.url is required"))
	}
	if c.Gate.Enabled && c.Gate.PolicyFile == "" {
		errs = append(errs, errors.New("gate.policy_file is required when gate.enabled is true"))
	}
	if c.Gate.Mode != GateModePerService && c.Gate.Mode != GateModeAbortCycle {
		errs = append(errs, fmt.Errorf("gate.mode must be %q or %q, got %q", GateModePerService, GateModeAbortCycle, c.Gate.Mode))
	}
	if c.PollInterval < minDuration {
		errs = append(errs, fmt.Errorf("poll_interval must be at least %s, got %s", minDuration, c.PollInterval))
	}
	if c.ConvergeTimeout < minDuration {
		errs = append(errs, fmt.Errorf("converge_timeout must be at least %s, got %s", minDuration, c.ConvergeTimeout))
	}
	if c.StageTimeout < minDuration {
		errs = append(errs, fmt.Errorf("stage_timeout must be at least %s, got %s", minDuration, c.StageTimeout))
	}
	for _, root := range c.VolumeBindRoots {
		if root == "/" {
			errs = append(errs, errors.New("volume_bind_roots must not contain /; use volume_bind_mounts for an exact read-only root mount"))
		}
	}
	for _, mount := range c.VolumeBindMounts {
		if mount.Source == "" {
			errs = append(errs, errors.New("volume_bind_mounts.source is required"))
		}
		if !mount.ReadOnly {
			errs = append(errs, fmt.Errorf("volume_bind_mounts source %q must set read_only: true", mount.Source))
		}
	}
	return errs
}

func parseDuration(name, v string, def time.Duration) (time.Duration, error) {
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", name, v, err)
	}
	return d, nil
}

func defaultString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func defaultBool(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

// splitCommaList parses a comma-separated env override into a trimmed,
// non-empty-entry slice; "" becomes nil (no entries), not [""].
func splitCommaList(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
