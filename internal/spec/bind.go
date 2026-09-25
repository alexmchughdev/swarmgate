package spec

import (
	"fmt"
	"os"
	"path/filepath"
)

// BindMountAllowance authorizes one exact bind source with its required
// read-only mode. It is intentionally separate from VolumeBindRoots: in
// particular, allowing "/" as a root would allow every host path.
type BindMountAllowance struct {
	Source   string
	ReadOnly bool
}

// resolvePathWithinRoot resolves path and reports whether it is contained by
// an allowed directory root or exactly equals an allowed non-directory entry.
// Both the candidate and entries are symlink-resolved before comparison.
func resolvePathWithinRoot(path string, roots []string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("bind source %q must be an absolute path", path)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve bind source %q: %w", path, err)
	}
	for _, root := range roots {
		if filepath.Clean(root) == string(filepath.Separator) {
			return "", fmt.Errorf("volume_bind_roots entry %q is forbidden; use an exact read-only volume_bind_mounts entry instead", root)
		}
		rootReal, err := filepath.EvalSymlinks(root)
		if err != nil {
			return "", fmt.Errorf("resolve volume_bind_roots entry %q: %w", root, err)
		}
		if filepath.Clean(rootReal) == string(filepath.Separator) {
			return "", fmt.Errorf("volume_bind_roots entry %q resolves to /; use an exact read-only volume_bind_mounts entry instead", root)
		}
		info, err := os.Stat(rootReal)
		if err != nil {
			return "", fmt.Errorf("stat volume_bind_roots entry %q: %w", root, err)
		}
		if !info.IsDir() {
			if realPath == rootReal {
				return realPath, nil
			}
			continue
		}
		rel, err := filepath.Rel(rootReal, realPath)
		if err == nil && rel != ".." && (len(rel) < 3 || rel[:3] != ".."+string(filepath.Separator)) {
			return realPath, nil
		}
	}
	return "", fmt.Errorf("bind source %q resolves to %q outside configured volume_bind_roots", path, realPath)
}

func resolveExactBindMount(path string, readOnly bool, allowances []BindMountAllowance) (string, bool, error) {
	if !filepath.IsAbs(path) {
		return "", false, fmt.Errorf("bind source %q must be an absolute path", path)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false, fmt.Errorf("resolve bind source %q: %w", path, err)
	}
	for _, allowance := range allowances {
		allowed, err := filepath.EvalSymlinks(allowance.Source)
		if err != nil {
			return "", false, fmt.Errorf("resolve volume_bind_mounts entry %q: %w", allowance.Source, err)
		}
		if realPath == allowed && readOnly == allowance.ReadOnly {
			return realPath, true, nil
		}
	}
	return realPath, false, nil
}
