package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/alexmchughdev/swarmgate/internal/source"
)

// resolveStackEnvFiles rewrites compose env_file paths to validated absolute
// host paths. Stack content itself remains in memory; only these explicitly
// configured host files are read by compose-go.
func resolveStackEnvFiles(f source.StackFile, root string) (source.StackFile, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(f.Content, &doc); err != nil {
		return f, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return f, nil
	}
	services := mappingValue(doc.Content[0], "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return f, nil
	}
	var paths []*yaml.Node
	for i := 0; i+1 < len(services.Content); i += 2 {
		svc := resolveAlias(services.Content[i+1])
		if svc.Kind != yaml.MappingNode {
			continue
		}
		envFile := mappingValue(svc, "env_file")
		if envFile == nil {
			continue
		}
		collectEnvFilePaths(envFile, &paths)
	}
	for _, p := range paths {
		if root == "" {
			return f, fmt.Errorf("env_file is not supported unless env_file_root is configured")
		}
		candidate := p.Value
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(root, filepath.FromSlash(candidate))
		}
		real, err := resolveEnvFileWithinRoot(candidate, root)
		if err != nil {
			if strings.Contains(err.Error(), "escapes allowed root") {
				return f, fmt.Errorf("env_file path %q escapes env_file_root", p.Value)
			}
			return f, fmt.Errorf("env_file path %q: %w", p.Value, err)
		}
		info, err := os.Stat(real)
		if err != nil {
			return f, fmt.Errorf("env_file %q: %w", p.Value, err)
		}
		if !info.Mode().IsRegular() {
			return f, fmt.Errorf("env_file %q is not a regular file", p.Value)
		}
		p.Value = real
	}
	if len(paths) == 0 {
		return f, nil
	}
	content, err := yaml.Marshal(&doc)
	if err != nil {
		return f, err
	}
	f.Content = content
	return f, nil
}

// resolveEnvFileWithinRoot resolves symlinks in both the env_file root and
// candidate, then checks containment using the resolved absolute paths.
func resolveEnvFileWithinRoot(candidate, root string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve allowed root: %w", err)
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("resolve allowed root %q: %w", root, err)
	}
	rootInfo, err := os.Stat(rootReal)
	if err != nil {
		return "", fmt.Errorf("inspect allowed root %q: %w", root, err)
	}
	if !rootInfo.IsDir() {
		return "", fmt.Errorf("allowed root %q is not a directory", root)
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	real, err := filepath.EvalSymlinks(candidateAbs)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q: %w", candidate, err)
	}
	rel, err := filepath.Rel(rootReal, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("resolved path %q escapes allowed root %q", candidate, root)
	}
	return real, nil
}

func mappingValue(n *yaml.Node, key string) *yaml.Node {
	n = resolveAlias(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return resolveAlias(n.Content[i+1])
		}
	}
	return nil
}

func collectEnvFilePaths(n *yaml.Node, out *[]*yaml.Node) {
	n = resolveAlias(n)
	switch n.Kind {
	case yaml.ScalarNode:
		*out = append(*out, n)
	case yaml.SequenceNode:
		for _, item := range n.Content {
			collectEnvFilePaths(item, out)
		}
	case yaml.MappingNode:
		if p := mappingValue(n, "path"); p != nil && p.Kind == yaml.ScalarNode {
			*out = append(*out, p)
		}
	}
}
