package spec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/distribution/reference"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/opencontainers/go-digest"

	"github.com/alexmchughdev/swarmgate/internal/secretfile"
)

// Resolve pins every tagged image in d to its registry digest, rewriting
// Image to repo@sha256:... form in the engine's familiar notation (e.g.
// nginx@sha256:..., not index.docker.io/library/nginx@sha256:...): the
// engine stores service images in familiar form, so any other spelling
// produces a permanent phantom diff against the observed state.
// Already-pinned references are canonicalised to the same familiar form.
//
// Per-service failures are collected and returned joined; services that did
// resolve keep their rewritten Image. The caller aborts the cycle on any
// error, so a partially rewritten DesiredState is never applied.
func Resolve(ctx context.Context, d *DesiredState, authFile string) error {
	kc, err := Keychain(authFile)
	if err != nil {
		return fmt.Errorf("load auth file: %w", err)
	}

	// Sorted iteration keeps error ordering stable across runs.
	names := make([]string, 0, len(d.Services))
	for n := range d.Services {
		names = append(names, n)
	}
	sort.Strings(names)

	var errs []error
	for _, n := range names {
		svc := d.Services[n]
		var pinned string
		var err error
		if strings.Contains(svc.Image, "@sha256:") {
			pinned, err = familiarRef(svc.Image)
		} else {
			pinned, err = resolveImage(ctx, svc.Image, kc)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("service %s: image %s: %w", n, svc.Image, err))
			continue
		}
		// Map values are copies; reassign to store the rewrite.
		svc.Image = pinned
		d.Services[n] = svc
	}
	return errors.Join(errs...)
}

func resolveImage(ctx context.Context, image string, kc authn.Keychain) (string, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return "", fmt.Errorf("parse reference: %w", err)
	}
	desc, err := remote.Head(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(kc))
	if err != nil {
		return "", fmt.Errorf("resolve digest: %w", err)
	}
	return pinnedRef(image, desc.Digest.String())
}

// pinnedRef renders image pinned to dgst in the engine's familiar notation.
func pinnedRef(image, dgst string) (string, error) {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", fmt.Errorf("parse reference: %w", err)
	}
	canonical, err := reference.WithDigest(reference.TrimNamed(named), digest.Digest(dgst))
	if err != nil {
		return "", fmt.Errorf("attach digest: %w", err)
	}
	return reference.FamiliarString(canonical), nil
}

// familiarRef canonicalises an already-pinned reference without touching
// the registry.
func familiarRef(image string) (string, error) {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", fmt.Errorf("parse reference: %w", err)
	}
	return reference.FamiliarString(named), nil
}

// Keychain builds the credential source Resolve uses to authenticate
// against registries, shared with any other package that needs to pull
// image metadata (e.g. internal/gate's signature verification).
func Keychain(authFile string) (authn.Keychain, error) {
	if authFile == "" {
		return authn.DefaultKeychain, nil
	}
	if err := secretfile.CheckPrivate(authFile); err != nil {
		return nil, fmt.Errorf("registry auth file: %w", err)
	}
	data, err := os.ReadFile(authFile)
	if err != nil {
		return nil, err
	}
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", authFile, err)
	}
	return fileKeychain{auths: cfg.Auths}, nil
}

// dockerConfig is the subset of docker's config.json we need. Parsing it
// directly avoids pulling in docker/cli just for credential lookup.
type dockerConfig struct {
	Auths map[string]dockerAuth `json:"auths"`
}

type dockerAuth struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

// fileKeychain resolves credentials from a parsed docker config.json.
type fileKeychain struct {
	auths map[string]dockerAuth
}

func (k fileKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	// Docker config keys are inconsistent in the wild: bare hosts, https://
	// prefixed hosts, and the legacy Docker Hub key all occur.
	candidates := []string{
		res.RegistryStr(),
		res.String(),
		"https://" + res.RegistryStr(),
		"https://" + res.String(),
	}
	if res.RegistryStr() == name.DefaultRegistry {
		candidates = append(candidates, "https://index.docker.io/v1/")
	}
	for _, key := range candidates {
		a, ok := k.auths[key]
		if !ok {
			continue
		}
		cfg, err := a.authConfig()
		if err != nil {
			return nil, fmt.Errorf("auth entry %s: %w", key, err)
		}
		return authn.FromConfig(cfg), nil
	}
	return authn.Anonymous, nil
}

func (a dockerAuth) authConfig() (authn.AuthConfig, error) {
	cfg := authn.AuthConfig{
		Username:      a.Username,
		Password:      a.Password,
		IdentityToken: a.IdentityToken,
	}
	if a.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(a.Auth)
		if err != nil {
			return authn.AuthConfig{}, fmt.Errorf("decode auth: %w", err)
		}
		user, pass, ok := strings.Cut(string(decoded), ":")
		if !ok {
			return authn.AuthConfig{}, errors.New("auth is not user:pass")
		}
		cfg.Username, cfg.Password = user, pass
	}
	return cfg, nil
}
