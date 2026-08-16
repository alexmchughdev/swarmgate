package harness

import (
	"context"
	"fmt"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"

	"github.com/alexmchughdev/swarmgate/internal/telemetry"
)

// gitPushAuthKeyEnv names the environment variable gitCommitAndPush checks
// for an SSH private key file to push with. Unset (the common case: every
// scenario's clone is a local-path remote, which the ssh transport is
// never invoked for regardless of this) preserves the original nil-auth
// behavior exactly. Set it whenever a campaign's remote is an ssh:// URL —
// go-git's own push, unlike the git CLI, does not read GIT_SSH_COMMAND or
// fall back to anything but an ssh-agent absent explicit auth, and a
// harness process launched as a background campaign script has no agent.
const gitPushAuthKeyEnv = "SWARMGATE_HARNESS_SSH_KEY"

// gitSSHUser returns the user@ portion of an ssh:// remote URL, defaulting
// to "git" when the URL has none or fails to parse. Mirrors
// internal/source's sshUser (same reasoning: a URL that does specify a
// user is a deliberate choice, and the conventional git-hosting default
// only applies when none was given) — duplicated rather than shared across
// packages for a ten-line helper with no other coupling between them.
func gitSSHUser(rawURL string) string {
	const defaultUser = "git"
	u, err := neturl.Parse(rawURL)
	if err != nil || u.User == nil || u.User.Username() == "" {
		return defaultUser
	}
	return u.User.Username()
}

func gitPushAuth(repo *git.Repository) (transport.AuthMethod, error) {
	keyFile := os.Getenv(gitPushAuthKeyEnv)
	if keyFile == "" {
		return nil, nil
	}
	user := "git"
	if remote, err := repo.Remote(git.DefaultRemoteName); err == nil {
		if urls := remote.Config().URLs; len(urls) > 0 {
			user = gitSSHUser(urls[0])
		}
	}
	keys, err := ssh.NewPublicKeysFromFile(user, keyFile, "")
	if err != nil {
		return nil, fmt.Errorf("load ssh key %s (from %s): %w", keyFile, gitPushAuthKeyEnv, err)
	}
	return keys, nil
}

// t1Tags are the pinned nginx tags t1 cycles through when bumping a
// service's image. Fixed and small so a campaign's image set stays within
// what's already cached locally/on the registry across runs.
var t1Tags = []string{"1.24-alpine", "1.25-alpine", "1.26-alpine", "1.27-alpine", "1.28-alpine"}

// imageRef prefixes name:tag with registry when set. A bare name:tag
// implies Docker Hub to both the Docker engine and, critically, to
// swarmgate's own resolve stage (a direct network call via
// go-containerregistry, independent of the engine and any registry
// mirror it might be configured with) — every resolve during a campaign
// would otherwise be a real WAN round trip to docker.io, contaminating
// exactly the timing this harness exists to measure. Empty registry
// preserves the original Docker Hub reference unchanged.
func imageRef(registry, name, tag string) string {
	if registry == "" {
		return name + ":" + tag
	}
	return registry + "/" + name + ":" + tag
}

// evalWorkloadHealthcheck matches swarmgate-eval-infra/workload/image's own
// probe contract exactly (params.env: PROBE_INTERVAL=2 PROBE_TIMEOUT=2
// PROBE_RETRIES=30, workload/compose/render.sh's healthcheck block): the
// image ships no shell, so the binary probes itself via `-healthcheck`.
// This is not cosmetic — Swarm holds a task in "starting" (not "running")
// until a defined healthcheck passes, so omitting this block would let
// every task report running almost immediately regardless of the image's
// own HEALTHY_AFTER delay, defeating the reason that delay exists: making
// convergence duration deterministic and controllable.
const evalWorkloadHealthcheck = "    healthcheck:\n" +
	"      test: [\"CMD\", \"/app\", \"-healthcheck\"]\n" +
	"      interval: 2s\n" +
	"      timeout: 2s\n" +
	"      retries: 30\n" +
	"      start_period: 0s\n"

// T1Config configures one t1 convergence campaign.
type T1Config struct {
	Repo    string // path to an existing working-tree clone
	Stack   string // stack name; file is <Stack>.yaml at the repo root
	Scale   int    // number of services in the stack
	Changes int    // number of services bumped per run
	Push    bool   // push after commit; false = commit only
	// Registry, Image, and Tags together build each service's image
	// reference (see imageRef); Image empty defaults to "nginx" and Tags
	// empty defaults to t1Tags, preserving the original nginx-only
	// behavior when neither is set.
	Registry    string
	Image       string
	Tags        []string
	Healthcheck bool // emit evalWorkloadHealthcheck on every generated service; see its doc comment
	EventsFile  string
	Timeout     time.Duration
	Label       string // expected form "events=on" / "events=off"; folded verbatim into Condition
}

// t1Service is one rendered service entry.
type t1Service struct {
	Name        string
	Image       string
	Healthcheck bool
}

func t1ServiceName(i int) string {
	return fmt.Sprintf("web%d", i+1)
}

// t1Plan derives this run's service set from the previous run's tag-cycle
// positions, advancing the first Changes services (by index) to the next
// pinned tag and wrapping around. A nil or wrongly-sized tagIndex is
// treated as a fresh stack, so the first-ever call needs no special case.
// Pure and deterministic: no I/O, so template generation and tag cycling
// are unit-testable without a repository or cluster.
func t1Plan(scale, changes int, tagIndex []int, registry, image string, tags []string, healthcheck bool) (services []t1Service, nextTagIndex []int) {
	if image == "" {
		image = "nginx"
	}
	if len(tags) == 0 {
		tags = t1Tags
	}
	if len(tagIndex) != scale {
		tagIndex = make([]int, scale)
	}
	next := append([]int(nil), tagIndex...)
	for i := 0; i < changes && i < scale; i++ {
		next[i] = (next[i] + 1) % len(tags)
	}
	services = make([]t1Service, scale)
	for i := 0; i < scale; i++ {
		services[i] = t1Service{Name: t1ServiceName(i), Image: imageRef(registry, image, tags[next[i]]), Healthcheck: healthcheck}
	}
	return services, next
}

// t1StackYAML renders services as a compose stack file.
func t1StackYAML(services []t1Service) string {
	var b strings.Builder
	b.WriteString("services:\n")
	for _, s := range services {
		fmt.Fprintf(&b, "  %s:\n    image: %s\n    deploy:\n      replicas: 1\n", s.Name, s.Image)
		if s.Healthcheck {
			b.WriteString(evalWorkloadHealthcheck)
		}
	}
	return b.String()
}

func t1Condition(cfg T1Config) string {
	return fmt.Sprintf("scale=%d;changes=%d;%s", cfg.Scale, cfg.Changes, cfg.Label)
}

// t1Runner carries cross-run state (each service's position in the tag
// cycle) for one campaign. State lives only in the process: a killed and
// restarted harness run resets every service to the first pinned tag
// rather than reading it back from the repository.
type t1Runner struct {
	cfg      T1Config
	tagIndex []int
}

func newT1Runner(cfg T1Config) *t1Runner {
	return &t1Runner{cfg: cfg}
}

func (r *t1Runner) run(runIdx int) Row {
	services, next := t1Plan(r.cfg.Scale, r.cfg.Changes, r.tagIndex, r.cfg.Registry, r.cfg.Image, r.cfg.Tags, r.cfg.Healthcheck)
	r.tagIndex = next

	stackPath := filepath.Join(r.cfg.Repo, r.cfg.Stack+".yaml")
	if err := os.WriteFile(stackPath, []byte(t1StackYAML(services)), 0o644); err != nil {
		return t1ErrorRow(runIdx, r.cfg, fmt.Errorf("write stack file: %w", err))
	}

	sha, err := gitCommitAndPush(r.cfg.Repo, r.cfg.Stack+".yaml", fmt.Sprintf("t1 run %d", runIdx), r.cfg.Push)
	if err != nil {
		return t1ErrorRow(runIdx, r.cfg, err)
	}

	tStart := time.Now()
	match := func(e telemetry.Event) bool {
		return e.Stage == telemetry.StageConverged && e.Fields["commit"] == sha
	}
	e, err := AwaitEvent(context.Background(), r.cfg.EventsFile, match, r.cfg.Timeout)
	tEnd := time.Now()
	outcome, detail := "ok", ""
	if err != nil {
		outcome, detail = "timeout", err.Error()
	} else {
		tEnd = e.T
	}
	return Row{
		Scenario: "t1", Condition: t1Condition(r.cfg), Run: runIdx,
		TStart: tStart, TEnd: tEnd, DurationMS: tEnd.Sub(tStart).Milliseconds(),
		Outcome: outcome, Detail: detail,
	}
}

func t1ErrorRow(runIdx int, cfg T1Config, err error) Row {
	now := time.Now()
	return Row{
		Scenario: "t1", Condition: t1Condition(cfg), Run: runIdx,
		TStart: now, TEnd: now, DurationMS: 0, Outcome: "error", Detail: err.Error(),
	}
}

// RunT1 executes the t1 convergence campaign: n runs, each generating a
// scale-service stack, bumping changes services' image tags, committing
// (and optionally pushing), and measuring time to the matching converged
// event.
func RunT1(cfg T1Config, n int, out *CSVWriter) error {
	r := newT1Runner(cfg)
	return Run(out, n, r.run)
}

// gitCommitAndPush stages path in the working-tree repo at repoDir, commits
// it, and pushes when push is true (NoErrAlreadyUpToDate is not an error:
// a no-op push is expected on a local-remote setup with a single ref).
// Returns the new commit's SHA.
func gitCommitAndPush(repoDir, path, message string, push bool) (string, error) {
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		return "", fmt.Errorf("open repo %s: %w", repoDir, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("worktree: %w", err)
	}
	if _, err := wt.Add(path); err != nil {
		return "", fmt.Errorf("add %s: %w", path, err)
	}
	hash, err := wt.Commit(message, &git.CommitOptions{
		Author: &object.Signature{Name: "harness", Email: "harness@swarmgate.local", When: time.Now()},
	})
	if err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	if push {
		auth, err := gitPushAuth(repo)
		if err != nil {
			return "", fmt.Errorf("push: %w", err)
		}
		if err := repo.Push(&git.PushOptions{Auth: auth}); err != nil && err != git.NoErrAlreadyUpToDate {
			return "", fmt.Errorf("push: %w", err)
		}
	}
	return hash.String(), nil
}
