package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	neturl "net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/alexmchughdev/swarmgate/internal/secretfile"
)

// maxStackFileSize bounds a single stack file's decompressed content. Without
// it, a single oversized blob at the configured path — pushed by anyone with
// write access to the branch, or served by a compromised/malicious remote —
// would be read entirely into memory before Parse ever sees it.
const maxStackFileSize = 1 << 20 // 1 MiB

// cloneDepth limits clone/fetch to the tip commit's objects rather than full
// history. Stack files are only ever read at the branch head (see Fetch), so
// history depth buys nothing while leaving a compromised or careless remote
// free to balloon an in-memory clone with an unbounded number of historical
// objects.
const cloneDepth = 1

// gitURLUserinfo matches the userinfo component of a URL (scheme://user[:pass]@host/...)
// or git's scp-like scp syntax (user@host:path), capturing an optional scheme
// prefix so it can be preserved.
var gitURLUserinfo = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.-]*://)?[^/@\s]+@`)

// redactURL replaces any embedded credential in a git remote URL with a fixed
// placeholder before it can reach an error message, log line, or telemetry
// event. A URL with no userinfo (including a local filesystem path) is
// returned unchanged.
func redactURL(raw string) string {
	return gitURLUserinfo.ReplaceAllString(raw, "$1***@")
}

// Commit identifies the repository head a set of stack files was read from.
type Commit struct {
	SHA  string
	When time.Time
}

// StackFile is one desired-state stack definition. Name is the filename with
// its .yml/.yaml extension stripped and doubles as the stack name.
type StackFile struct {
	Name    string
	Content []byte
}

// GitSource polls a Git repository for stack files. The repository is cloned
// bare into in-memory storage on the first Fetch, so no on-disk state is kept
// and no cleanup is required; later Fetch calls only transfer new objects.
type GitSource struct {
	url        string
	branch     string
	path       string
	sshKeyFile string

	repo *git.Repository
}

// NewGitSource returns a source that reads *.yml and *.yaml files under path
// (non-recursive) at the head of branch in the repository at url. If
// sshKeyFile is non-empty it is used for SSH public-key auth as the user
// named in url's ssh://user@host form, or "git" if url specifies none;
// otherwise go-git's default auth chain applies.
func NewGitSource(url, branch, path, sshKeyFile string) *GitSource {
	return &GitSource{url: url, branch: branch, path: path, sshKeyFile: sshKeyFile}
}

// Fetch brings the local copy up to date with the remote branch and returns
// the head commit together with the stack files at that commit, sorted by
// filename.
func (s *GitSource) Fetch(ctx context.Context) (Commit, []StackFile, error) {
	if s.repo == nil {
		if err := s.clone(ctx); err != nil {
			return Commit{}, nil, err
		}
	} else if err := s.update(ctx); err != nil {
		return Commit{}, nil, err
	}

	ref, err := s.repo.Reference(plumbing.NewBranchReferenceName(s.branch), true)
	if err != nil {
		return Commit{}, nil, fmt.Errorf("resolve branch %q: %w", s.branch, err)
	}
	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return Commit{}, nil, fmt.Errorf("read commit %s: %w", ref.Hash(), err)
	}
	files, err := s.stackFiles(commit)
	if err != nil {
		return Commit{}, nil, err
	}
	return Commit{SHA: ref.Hash().String(), When: commit.Committer.When}, files, nil
}

func (s *GitSource) clone(ctx context.Context) error {
	auth, err := s.auth()
	if err != nil {
		return err
	}
	repo, err := git.CloneContext(ctx, memory.NewStorage(), nil, &git.CloneOptions{
		URL:           s.url,
		ReferenceName: plumbing.NewBranchReferenceName(s.branch),
		SingleBranch:  true,
		Depth:         cloneDepth,
		Auth:          auth,
	})
	if err != nil {
		return fmt.Errorf("clone %s: %w", redactURL(s.url), err)
	}
	s.repo = repo
	return nil
}

func (s *GitSource) update(ctx context.Context) error {
	auth, err := s.auth()
	if err != nil {
		return err
	}
	// Force-update the local branch ref directly: the repository is bare, so
	// there is no worktree to advance via checkout, and clone's default
	// refspec would only move the remote-tracking ref.
	spec := gitconfig.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/heads/%s", s.branch, s.branch))
	err = s.repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: git.DefaultRemoteName,
		RefSpecs:   []gitconfig.RefSpec{spec},
		Depth:      cloneDepth,
		Auth:       auth,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("fetch %s: %w", redactURL(s.url), err)
	}
	return nil
}

func (s *GitSource) auth() (transport.AuthMethod, error) {
	if s.sshKeyFile == "" {
		return nil, nil
	}
	if err := secretfile.CheckPrivate(s.sshKeyFile); err != nil {
		return nil, fmt.Errorf("ssh key file: %w", err)
	}
	keys, err := ssh.NewPublicKeysFromFile(sshUser(s.url), s.sshKeyFile, "")
	if err != nil {
		return nil, fmt.Errorf("load ssh key %s: %w", s.sshKeyFile, err)
	}
	return keys, nil
}

// sshUser returns the user@ portion of an ssh:// url, defaulting to "git"
// (the conventional git-hosting username, and this function's own prior
// hardcoded behavior) when the url has none or fails to parse. A URL that
// does specify a user is a deliberate operator choice — e.g. pointing at a
// plain SSH login rather than a dedicated git-hosting account — and
// silently overriding it to "git" regardless would make that choice
// unrepresentable in config.
func sshUser(rawURL string) string {
	const defaultUser = "git"
	u, err := neturl.Parse(rawURL)
	if err != nil || u.User == nil || u.User.Username() == "" {
		return defaultUser
	}
	return u.User.Username()
}

// readLimited reads file's content, refusing anything past limit bytes. It
// doesn't trust file.Size alone: that value comes from the git object's own
// header, and reading is bounded independently in case a crafted object's
// header understates its true decompressed length.
func readLimited(file *object.File, limit int64) ([]byte, error) {
	reader, err := file.Reader()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	content, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("exceeds %d byte limit", limit)
	}
	return content, nil
}

func (s *GitSource) stackFiles(commit *object.Commit) ([]StackFile, error) {
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("read tree of %s: %w", commit.Hash, err)
	}
	dir := path.Clean(strings.Trim(s.path, "/"))
	if dir != "" && dir != "." {
		tree, err = tree.Tree(dir)
		if err != nil {
			return nil, fmt.Errorf("resolve path %q in %s: %w", dir, commit.Hash, err)
		}
	}

	var files []StackFile
	// seenBy maps a derived stack name back to the first filename that
	// produced it, so a second file mapping to the same name (e.g.
	// alpha.yml alongside alpha.yaml) is caught here rather than silently
	// overwriting or duplicating a stack downstream.
	seenBy := make(map[string]string)
	var errs []error
	for _, entry := range tree.Entries {
		if !entry.Mode.IsFile() {
			continue
		}
		ext := path.Ext(entry.Name)
		if ext != ".yml" && ext != ".yaml" {
			continue
		}
		name := strings.TrimSuffix(entry.Name, ext)
		if first, dup := seenBy[name]; dup {
			a, b := first, entry.Name
			if b < a {
				a, b = b, a
			}
			errs = append(errs, fmt.Errorf("stack name %q is ambiguous: both %s and %s map to it", name, a, b))
			continue
		}
		seenBy[name] = entry.Name

		file, err := tree.TreeEntryFile(&entry)
		if err != nil {
			return nil, fmt.Errorf("open %s at %s: %w", entry.Name, commit.Hash, err)
		}
		if file.Size > maxStackFileSize {
			return nil, fmt.Errorf("%s at %s: %d bytes exceeds the %d byte stack file limit", entry.Name, commit.Hash, file.Size, maxStackFileSize)
		}
		content, err := readLimited(file, maxStackFileSize)
		if err != nil {
			return nil, fmt.Errorf("read %s at %s: %w", entry.Name, commit.Hash, err)
		}
		files = append(files, StackFile{Name: name, Content: content})
	}
	if len(errs) > 0 {
		sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
		return nil, errors.Join(errs...)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}
