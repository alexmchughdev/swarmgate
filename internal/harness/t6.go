package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/google/go-containerregistry/pkg/crane"

	"github.com/alexmchughdev/swarmgate/internal/spec"
	"github.com/alexmchughdev/swarmgate/internal/telemetry"
)

// T6Cases are the valid --case values.
var T6Cases = []string{"ok", "unsigned", "wrong-identity", "no-attestation", "tag-repoint"}

// t6Variant maps a static --case to the pipeline image variant
// pipeline/build.sh pushes it against. tag-repoint has no static variant —
// it targets a dedicated tag the harness itself retags mid-run.
var t6Variant = map[string]string{
	"ok":             "ok-signed",
	"unsigned":       "unsigned",
	"wrong-identity": "wrong-identity",
	"no-attestation": "no-attestation",
}

// T6Config configures one t6 gate campaign.
type T6Config struct {
	Repo     string // path to an existing working-tree git clone
	Stack    string // stack name; file is <Stack>.yaml at the repo root
	Service  string // base service name; each run appends its own index
	Registry string // registry host:port pipeline/build.sh pushed the fixture images to
	Case     string // one of T6Cases
	// Mode is folded into Condition only; it names the gate.mode the
	// swarmgate instance under test is actually configured with, which
	// the harness has no control over and does not branch on. Comparing
	// per-service against abort-cycle behavior means running the same
	// --case against two differently configured swarmgate instances and
	// diffing the resulting rows/JSONL by hand (see scripts/README.md).
	Mode       string
	DockerHost string // docker engine host; only the tag-repoint case's post-hoc inspect needs it
	Push       bool
	EventsFile string
	Timeout    time.Duration
	Label      string
}

func t6Condition(cfg T6Config) string {
	return fmt.Sprintf("case=%s;mode=%s;%s", cfg.Case, cfg.Mode, cfg.Label)
}

func t6ServiceName(base string, run int) string {
	return fmt.Sprintf("%s-%d", base, run)
}

func t6Row(cfg T6Config, run int, tStart, tEnd time.Time, outcome, detail string) Row {
	return Row{
		Scenario: "t6", Condition: t6Condition(cfg), Run: run,
		TStart: tStart, TEnd: tEnd, DurationMS: tEnd.Sub(tStart).Milliseconds(),
		Outcome: outcome, Detail: detail,
	}
}

func t6ErrorRow(cfg T6Config, run int, detail string) Row {
	now := time.Now()
	return t6Row(cfg, run, now, now, "error", detail)
}

// t6Image returns the pipeline image reference cfg.Case's stack service
// should point at.
func t6Image(cfg T6Config) (string, error) {
	if cfg.Case == "tag-repoint" {
		return fmt.Sprintf("%s/swarmgate-test/tag-repoint:v1", cfg.Registry), nil
	}
	variant, ok := t6Variant[cfg.Case]
	if !ok {
		return "", fmt.Errorf("t6: unknown case %q", cfg.Case)
	}
	return fmt.Sprintf("%s/swarmgate-test/%s:v1", cfg.Registry, variant), nil
}

// t6SigTag renders digest (sha256:...) in the tag form cosign's new bundle
// format uses to locate a signed image's companion bundle artifact within
// its own repository.
func t6SigTag(digest string) string {
	return "sha256-" + strings.TrimPrefix(digest, "sha256:")
}

// t6API is the slice of the Docker client the tag-repoint case needs, to
// read back the actually-deployed image digest after convergence.
type t6API interface {
	ServiceInspectWithRaw(ctx context.Context, serviceID string, opts swarm.ServiceInspectOptions) (swarm.Service, []byte, error)
}

func newT6Client(host string) (t6API, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	c, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return c, nil
}

// t6DeployedDigest inspects the live service and returns the digest
// portion of its container image, in the engine-familiar repo@sha256:...
// form every applied service carries (spec.ToSwarm passes Image through
// unchanged from the already digest-pinned normal form).
func t6DeployedDigest(ctx context.Context, api t6API, service string) (string, error) {
	svc, _, err := api.ServiceInspectWithRaw(ctx, service, swarm.ServiceInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("inspect %q: %w", service, err)
	}
	if svc.Spec.TaskTemplate.ContainerSpec == nil {
		return "", fmt.Errorf("service %q has no container spec", service)
	}
	_, digest, ok := strings.Cut(svc.Spec.TaskTemplate.ContainerSpec.Image, "@")
	if !ok {
		return "", fmt.Errorf("service %q image %q is not digest-pinned", service, svc.Spec.TaskTemplate.ContainerSpec.Image)
	}
	return digest, nil
}

// awaitFromChannel drains events (opened by an earlier Tail call, before
// the action that produces the signal) until match returns true or ctx is
// done. Used instead of AwaitEvent when a run needs more than one signal
// from the same cycle in sequence — re-Tailing between them would risk
// missing an event that fires in the gap.
func awaitFromChannel(ctx context.Context, events <-chan telemetry.Event, match func(telemetry.Event) bool) (telemetry.Event, error) {
	for {
		select {
		case e, ok := <-events:
			if !ok {
				return telemetry.Event{}, fmt.Errorf("await event: channel closed")
			}
			if match(e) {
				return e, nil
			}
		case <-ctx.Done():
			return telemetry.Event{}, fmt.Errorf("await event: %w", ctx.Err())
		}
	}
}

type t6Runner struct {
	cfg T6Config
	api t6API // nil unless cfg.Case == "tag-repoint"
}

func newT6Runner(cfg T6Config) (*t6Runner, error) {
	if cfg.Case != "tag-repoint" {
		return &t6Runner{cfg: cfg}, nil
	}
	api, err := newT6Client(cfg.DockerHost)
	if err != nil {
		return nil, err
	}
	return &t6Runner{cfg: cfg, api: api}, nil
}

func (r *t6Runner) run(runIdx int) Row {
	cfg := r.cfg
	// The stack file declares services by their bare (unqualified) name;
	// swarmgate's own telemetry and the Docker engine both key on the
	// qualified <stack>_<service> form (spec.ServiceName) — matching
	// events or inspecting the deployed service must use the qualified
	// name, or every wait below silently never matches anything.
	bareService := t6ServiceName(cfg.Service, runIdx)
	qualifiedService := spec.ServiceName(cfg.Stack, bareService)

	image, err := t6Image(cfg)
	if err != nil {
		return t6ErrorRow(cfg, runIdx, err.Error())
	}

	if cfg.Case == "tag-repoint" {
		// Seed the shared tag-repoint:v1 tag with ok-signed's content
		// before this run's push, so the resolve event below captures a
		// digest known to be validly signed.
		seedRepo := fmt.Sprintf("%s/swarmgate-test/ok-signed", cfg.Registry)
		seedRef := seedRepo + ":v1"
		digest, err := crane.Digest(seedRef)
		if err != nil {
			return t6ErrorRow(cfg, runIdx, fmt.Sprintf("seed tag-repoint: digest: %v", err))
		}
		if err := crane.Copy(seedRef, image); err != nil {
			return t6ErrorRow(cfg, runIdx, fmt.Sprintf("seed tag-repoint: %v", err))
		}
		// cosign's new bundle format discovers a signature via a
		// sha256-<digest> tag in the SAME repository as the signed
		// image; crane.Copy only copies the image manifest itself, not
		// that companion artifact, so the seeded copy would otherwise
		// look unsigned under its new name despite being byte-identical
		// to a signed image.
		sigTag := t6SigTag(digest)
		sigSrc := fmt.Sprintf("%s:%s", seedRepo, sigTag)
		sigDst := fmt.Sprintf("%s/swarmgate-test/tag-repoint:%s", cfg.Registry, sigTag)
		if err := crane.Copy(sigSrc, sigDst); err != nil {
			return t6ErrorRow(cfg, runIdx, fmt.Sprintf("seed tag-repoint signature: %v", err))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	// Tail before push: the signals this run waits for are from the SAME
	// cycle the push causes, so listening must already be in place before
	// the push exists to be reconciled.
	events, err := Tail(ctx, cfg.EventsFile)
	if err != nil {
		return t6ErrorRow(cfg, runIdx, fmt.Sprintf("tail events: %v", err))
	}

	stackPath := filepath.Join(cfg.Repo, cfg.Stack+".yaml")
	stack := t1StackYAML([]t1Service{{Name: bareService, Image: image}})
	if err := os.WriteFile(stackPath, []byte(stack), 0o644); err != nil {
		return t6ErrorRow(cfg, runIdx, fmt.Sprintf("write stack file: %v", err))
	}
	sha, err := gitCommitAndPush(cfg.Repo, cfg.Stack+".yaml", fmt.Sprintf("t6 run %d", runIdx), cfg.Push)
	if err != nil {
		return t6ErrorRow(cfg, runIdx, err.Error())
	}
	tStart := time.Now()

	if cfg.Case == "tag-repoint" {
		return r.runTagRepoint(ctx, runIdx, qualifiedService, sha, events, tStart)
	}
	return r.runStaticCase(ctx, runIdx, qualifiedService, tStart, events, sha)
}

// runStaticCase handles ok/unsigned/wrong-identity/no-attestation: await
// this run's verify event and check its outcome against what the case
// expects. "ok" additionally awaits the matching converged event, per
// this scenario's Accept clause.
func (r *t6Runner) runStaticCase(ctx context.Context, runIdx int, service string, tStart time.Time, events <-chan telemetry.Event, sha string) Row {
	cfg := r.cfg
	verifyMatch := func(e telemetry.Event) bool {
		return e.Stage == telemetry.StageVerify && e.Service == service
	}
	ve, err := awaitFromChannel(ctx, events, verifyMatch)
	if err != nil {
		return t6Row(cfg, runIdx, tStart, time.Now(), "timeout", "verify: "+err.Error())
	}

	gotOutcome, _ := ve.Fields["outcome"].(string)
	wantOutcome := "pass"
	if cfg.Case != "ok" {
		wantOutcome = "reject"
	}
	if gotOutcome != wantOutcome {
		reason, _ := ve.Fields["reason"].(string)
		return t6Row(cfg, runIdx, tStart, ve.T, "error",
			fmt.Sprintf("verify=%s;want=%s;reason=%s", gotOutcome, wantOutcome, reason))
	}
	if cfg.Case != "ok" {
		return t6Row(cfg, runIdx, tStart, ve.T, "ok", "verify="+gotOutcome)
	}

	convMatch := func(e telemetry.Event) bool {
		return e.Stage == telemetry.StageConverged && e.Fields["commit"] == sha
	}
	ce, err := awaitFromChannel(ctx, events, convMatch)
	if err != nil {
		return t6Row(cfg, runIdx, tStart, time.Now(), "timeout", "verify=pass;converged=timeout")
	}
	return t6Row(cfg, runIdx, tStart, ce.T, "ok", "verify=pass;converged=true")
}

// runTagRepoint waits for this run's resolve event, captures the digest
// swarmgate pinned, then retags tag-repoint:v1 onto unsigned's (distinct,
// see pipeline/README.md) content and waits for convergence. The security
// property under test: the deployed digest must equal the resolved one,
// proving swarmgate deploys what it pinned rather than re-resolving the
// tag at apply time.
func (r *t6Runner) runTagRepoint(ctx context.Context, runIdx int, service, sha string, events <-chan telemetry.Event, tStart time.Time) Row {
	cfg := r.cfg
	resolveMatch := func(e telemetry.Event) bool {
		return e.Stage == telemetry.StageResolve && e.Service == service
	}
	re, err := awaitFromChannel(ctx, events, resolveMatch)
	if err != nil {
		return t6Row(cfg, runIdx, tStart, time.Now(), "timeout", "resolve: "+err.Error())
	}
	resolvedDigest, _ := re.Fields["digest"].(string)
	if resolvedDigest == "" {
		return t6Row(cfg, runIdx, tStart, re.T, "error", "resolve event carried no digest")
	}

	unsignedRef := fmt.Sprintf("%s/swarmgate-test/unsigned:v1", cfg.Registry)
	tagRepointRef := fmt.Sprintf("%s/swarmgate-test/tag-repoint:v1", cfg.Registry)
	if err := crane.Copy(unsignedRef, tagRepointRef); err != nil {
		return t6Row(cfg, runIdx, tStart, re.T, "error", fmt.Sprintf("retag: %v", err))
	}

	convMatch := func(e telemetry.Event) bool {
		return e.Stage == telemetry.StageConverged && e.Fields["commit"] == sha
	}
	ce, err := awaitFromChannel(ctx, events, convMatch)
	if err != nil {
		return t6Row(cfg, runIdx, tStart, time.Now(), "timeout", fmt.Sprintf("resolved=%s;converged=timeout", resolvedDigest))
	}

	deployed, err := t6DeployedDigest(context.Background(), r.api, service)
	if err != nil {
		return t6Row(cfg, runIdx, tStart, ce.T, "error", fmt.Sprintf("resolved=%s;inspect_error=%v", resolvedDigest, err))
	}
	if deployed != resolvedDigest {
		return t6Row(cfg, runIdx, tStart, ce.T, "error",
			fmt.Sprintf("resolved=%s;deployed=%s;match=false", resolvedDigest, deployed))
	}
	return t6Row(cfg, runIdx, tStart, ce.T, "ok",
		fmt.Sprintf("resolved=%s;deployed=%s;match=true", resolvedDigest, deployed))
}

// RunT6 executes the t6 gate campaign: n runs, each pointing a freshly
// (uniquely) named stack service at the image cfg.Case selects, pushing,
// and recording the observed gate outcome.
func RunT6(cfg T6Config, n int, out *CSVWriter) error {
	if cfg.Case != "tag-repoint" {
		if _, ok := t6Variant[cfg.Case]; !ok {
			return fmt.Errorf("t6: unknown case %q", cfg.Case)
		}
	}
	r, err := newT6Runner(cfg)
	if err != nil {
		return err
	}
	return Run(out, n, r.run)
}
