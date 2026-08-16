package spec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// startRegistry runs an in-process registry and pushes a random image to
// <host>/repo:v1, returning the host and the pushed image's digest.
func startRegistry(t *testing.T) (host string, digest v1.Hash) {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	host = u.Host

	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	digest, err = img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	tag, err := name.NewTag(host + "/repo:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	return host, digest
}

func TestResolvePinsTag(t *testing.T) {
	host, digest := startRegistry(t)
	d := &DesiredState{Services: map[string]ServiceSpec{
		"app_web": {Name: "app_web", Image: host + "/repo:v1"},
	}}
	if err := Resolve(context.Background(), d, ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := host + "/repo@" + digest.String()
	if got := d.Services["app_web"].Image; got != want {
		t.Fatalf("Image = %q, want %q", got, want)
	}
}

func TestResolvePinnedPassesThrough(t *testing.T) {
	host, digest := startRegistry(t)
	pinned := host + "/repo@" + digest.String()
	d := &DesiredState{Services: map[string]ServiceSpec{
		"app_web": {Name: "app_web", Image: pinned},
	}}
	if err := Resolve(context.Background(), d, ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := d.Services["app_web"].Image; got != pinned {
		t.Fatalf("Image = %q, want untouched %q", got, pinned)
	}
}

func TestResolveMixed(t *testing.T) {
	host, digest := startRegistry(t)
	pinned := host + "/repo@" + digest.String()
	d := &DesiredState{Services: map[string]ServiceSpec{
		"app_web": {Name: "app_web", Image: host + "/repo:v1"},
		"app_db":  {Name: "app_db", Image: pinned},
	}}
	if err := Resolve(context.Background(), d, ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, want := d.Services["app_web"].Image, host+"/repo@"+digest.String(); got != want {
		t.Fatalf("tagged Image = %q, want %q", got, want)
	}
	if got := d.Services["app_db"].Image; got != pinned {
		t.Fatalf("pinned Image = %q, want untouched %q", got, pinned)
	}
}

func TestResolveUnresolvable(t *testing.T) {
	host, digest := startRegistry(t)
	d := &DesiredState{Services: map[string]ServiceSpec{
		"app_web": {Name: "app_web", Image: host + "/repo:v1"},
		"app_bad": {Name: "app_bad", Image: host + "/missing:v1"},
	}}
	err := Resolve(context.Background(), d, "")
	if err == nil {
		t.Fatal("Resolve: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "app_bad") {
		t.Fatalf("error %q does not mention service app_bad", err)
	}
	// Successes are still rewritten despite the partial failure.
	if got, want := d.Services["app_web"].Image, host+"/repo@"+digest.String(); got != want {
		t.Fatalf("Image = %q, want %q", got, want)
	}
}

func TestResolveWithAuthFile(t *testing.T) {
	host, digest := startRegistry(t)
	cfg := map[string]any{
		"auths": map[string]any{
			host: map[string]string{
				"auth": base64.StdEncoding.EncodeToString([]byte("user:pass")),
			},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	d := &DesiredState{Services: map[string]ServiceSpec{
		"app_web": {Name: "app_web", Image: host + "/repo:v1"},
	}}
	if err := Resolve(context.Background(), d, path); err != nil {
		t.Fatalf("Resolve with auth file: %v", err)
	}
	if got, want := d.Services["app_web"].Image, host+"/repo@"+digest.String(); got != want {
		t.Fatalf("Image = %q, want %q", got, want)
	}
}

// TestResolveRejectsGroupReadableAuthFile is a regression test: a registry
// auth file holds credentials, so Resolve must refuse one left readable by
// group or other rather than silently trusting whatever mode it happens to
// have on disk.
func TestResolveRejectsGroupReadableAuthFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"auths":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &DesiredState{Services: map[string]ServiceSpec{
		"app_web": {Name: "app_web", Image: "example.com/repo:v1"},
	}}
	err := Resolve(context.Background(), d, path)
	if err == nil {
		t.Fatal("Resolve: expected an error for a group-readable auth file, got nil")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error %q does not mention the fix", err)
	}
}

func TestPinnedRefFamiliarForms(t *testing.T) {
	const dgst = "sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10"
	tests := []struct {
		image string
		want  string
	}{
		{"nginx:1.27-alpine", "nginx@" + dgst},
		{"library/nginx:1.27", "nginx@" + dgst},
		{"docker.io/library/nginx:1.27", "nginx@" + dgst},
		{"index.docker.io/library/nginx:1.27", "nginx@" + dgst},
		{"grafana/grafana:11.0.0", "grafana/grafana@" + dgst},
		{"registry.local:5000/team/app:v1", "registry.local:5000/team/app@" + dgst},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			got, err := pinnedRef(tt.image, dgst)
			if err != nil {
				t.Fatalf("pinnedRef: %v", err)
			}
			if got != tt.want {
				t.Fatalf("pinnedRef(%q) = %q, want %q", tt.image, got, tt.want)
			}
		})
	}
}

func TestFamiliarRefCanonicalisesPinned(t *testing.T) {
	const dgst = "sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10"
	got, err := familiarRef("index.docker.io/library/nginx@" + dgst)
	if err != nil {
		t.Fatalf("familiarRef: %v", err)
	}
	if want := "nginx@" + dgst; got != want {
		t.Fatalf("familiarRef = %q, want %q", got, want)
	}
}
