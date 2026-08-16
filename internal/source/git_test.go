package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

var testWhen = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func initTestRepo(t *testing.T) (string, *git.Worktree) {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	return dir, wt
}

// commitFiles writes the given files (keys are slash-separated repo-relative
// paths), stages them, and commits with a fixed signature.
func commitFiles(t *testing.T, dir string, wt *git.Worktree, msg string, files map[string]string) plumbing.Hash {
	t.Helper()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
	}
	hash, err := wt.Commit(msg, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: testWhen},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return hash
}

func assertStacks(t *testing.T, got []StackFile, want map[string]string, order []string) {
	t.Helper()
	if len(got) != len(order) {
		t.Fatalf("got %d stack files, want %d", len(got), len(order))
	}
	for i, sf := range got {
		if sf.Name != order[i] {
			t.Errorf("stack %d: got name %q, want %q", i, sf.Name, order[i])
		}
		if string(sf.Content) != want[sf.Name] {
			t.Errorf("stack %q: got content %q, want %q", sf.Name, sf.Content, want[sf.Name])
		}
	}
}

func TestFetchReturnsStackFilesSorted(t *testing.T) {
	dir, wt := initTestRepo(t)
	hash := commitFiles(t, dir, wt, "initial", map[string]string{
		"stacks/beta.yml":   "services: {}\n",
		"stacks/alpha.yaml": "services:\n  web: {}\n",
		"stacks/notes.txt":  "not a stack\n",
	})

	src := NewGitSource(dir, "master", "stacks", "")
	commit, files, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if commit.SHA != hash.String() {
		t.Errorf("got SHA %s, want %s", commit.SHA, hash)
	}
	if !commit.When.Equal(testWhen) {
		t.Errorf("got When %v, want %v", commit.When, testWhen)
	}
	assertStacks(t, files, map[string]string{
		"alpha": "services:\n  web: {}\n",
		"beta":  "services: {}\n",
	}, []string{"alpha", "beta"})
}

func TestFetchPicksUpNewCommits(t *testing.T) {
	dir, wt := initTestRepo(t)
	first := commitFiles(t, dir, wt, "initial", map[string]string{
		"stacks/alpha.yaml": "services:\n  web: {}\n",
		"stacks/beta.yml":   "services: {}\n",
	})

	src := NewGitSource(dir, "master", "stacks", "")
	commit, _, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	if commit.SHA != first.String() {
		t.Errorf("first Fetch: got SHA %s, want %s", commit.SHA, first)
	}

	second := commitFiles(t, dir, wt, "update", map[string]string{
		"stacks/beta.yml":   "services:\n  db: {}\n",
		"stacks/gamma.yaml": "services:\n  cache: {}\n",
	})

	commit, files, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if commit.SHA != second.String() {
		t.Errorf("second Fetch: got SHA %s, want %s", commit.SHA, second)
	}
	assertStacks(t, files, map[string]string{
		"alpha": "services:\n  web: {}\n",
		"beta":  "services:\n  db: {}\n",
		"gamma": "services:\n  cache: {}\n",
	}, []string{"alpha", "beta", "gamma"})
}

func TestFetchRejectsAmbiguousStackName(t *testing.T) {
	dir, wt := initTestRepo(t)
	commitFiles(t, dir, wt, "initial", map[string]string{
		"stacks/alpha.yaml": "services:\n  web: {}\n",
		"stacks/alpha.yml":  "services:\n  other: {}\n",
		"stacks/beta.yml":   "services: {}\n",
	})

	src := NewGitSource(dir, "master", "stacks", "")
	_, files, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatalf("Fetch() = %d files, nil error, want an ambiguous-stack-name error", len(files))
	}
	for _, want := range []string{`stack name "alpha"`, "alpha.yaml", "alpha.yml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestFetchRejectsOversizedStackFile(t *testing.T) {
	dir, wt := initTestRepo(t)
	commitFiles(t, dir, wt, "initial", map[string]string{
		"stacks/huge.yaml": strings.Repeat("a", maxStackFileSize+1),
	})

	src := NewGitSource(dir, "master", "stacks", "")
	_, files, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatalf("Fetch() = %d files, nil error, want a size-limit error", len(files))
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error %q does not mention the size limit", err)
	}
}

// TestFetchRejectsGroupReadableSSHKey is a regression test: an SSH private
// key is a secret, so Fetch must refuse one left group/world-readable
// rather than silently handing it to the SSH auth stack.
func TestFetchRejectsGroupReadableSSHKey(t *testing.T) {
	dir, wt := initTestRepo(t)
	commitFiles(t, dir, wt, "initial", map[string]string{
		"stacks/alpha.yaml": "services:\n  web: {}\n",
	})

	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("not a real key"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := NewGitSource(dir, "master", "stacks", keyPath)
	_, _, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch: expected an error for a group-readable ssh key file, got nil")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error %q does not mention the fix", err)
	}
}

func TestRedactURLStripsCredentials(t *testing.T) {
	cases := map[string]string{
		"https://oauth2:ghp_secrettoken@github.com/org/repo.git": "https://***@github.com/org/repo.git",
		"https://ghp_secrettoken@github.com/org/repo.git":        "https://***@github.com/org/repo.git",
		"ssh://git@host/repo.git":                                "ssh://***@host/repo.git",
		"git@host:org/repo.git":                                  "***@host:org/repo.git",
		"/local/path/bare.git":                                   "/local/path/bare.git",
		"https://github.com/org/repo.git":                        "https://github.com/org/repo.git",
	}
	for raw, want := range cases {
		if got := redactURL(raw); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestSSHUser(t *testing.T) {
	cases := map[string]string{
		"ssh://git@host/repo.git":    "git",
		"ssh://root@host/repo.git":   "root",
		"ssh://deploy@host:22/r.git": "deploy",
		"ssh://host/repo.git":        "git",
		"/local/path/bare.git":       "git",
		"not a valid url at all :::": "git",
		"https://host/org/repo.git":  "git",
	}
	for raw, want := range cases {
		if got := sshUser(raw); got != want {
			t.Errorf("sshUser(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestFetchScopedToSubdirectory(t *testing.T) {
	dir, wt := initTestRepo(t)
	commitFiles(t, dir, wt, "initial", map[string]string{
		"root.yaml":         "services:\n  outside: {}\n",
		"stacks/alpha.yaml": "services:\n  web: {}\n",
		"other/delta.yml":   "services:\n  other: {}\n",
	})

	src := NewGitSource(dir, "master", "stacks", "")
	_, files, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	assertStacks(t, files, map[string]string{
		"alpha": "services:\n  web: {}\n",
	}, []string{"alpha"})
}
