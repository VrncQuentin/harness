package tools

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// validatePath checks that path (after symlink resolution) is within at
// least one sandbox root. If sandbox roots are empty, all paths are rejected.
func validatePath(path string, roots []string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("tools: cannot resolve path: %w", err)
	}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		resolvedRoot, err := canonicalPath(filepath.Clean(root))
		if err != nil {
			return "", fmt.Errorf("tools: resolve sandbox root %s: %w", root, err)
		}
		resolvedAncestor, err := resolveExistingAncestor(abs)
		if err != nil {
			continue
		}
		if pathWithinRoot(resolvedAncestor, resolvedRoot) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrSandboxViolation, path)
}

// resolveExistingAncestor canonicalizes the deepest path component already
// present on disk, then re-appends the components below it. This prevents a
// lexically in-root missing target below a symlink or junction from escaping
// its sandbox: a file about to be created under a link still resolves through
// that link.
//
// Re-appending the tail is what makes the check sound. Returning only the
// existing ancestor would compare a path shorter than the request, so a
// not-yet-created target below an escaping link would be judged by its
// in-root parent instead of its real destination.
func resolveExistingAncestor(path string) (string, error) {
	current := filepath.Clean(path)
	var tail []string
	for {
		resolved, err := canonicalPath(current)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		tail = append(tail, filepath.Base(current))
		current = parent
	}
}

func pathWithinRoot(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
		root = strings.ToLower(root)
	}
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}
