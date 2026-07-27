package tools

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/VrncQuentin/harness/internal/pathid"
)

// validatePath checks that path (after physical resolution) is within at
// least one sandbox root. If sandbox roots are empty, all paths are rejected.
//
// A resolution failure rejects the call rather than moving on to the next
// root. An unresolvable path is not a path known to be outside the sandbox —
// it is a path whose location is unknown, and the sandbox is a security
// boundary, so the unknown case has to be a refusal.
func validatePath(path string, roots []string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("tools: cannot resolve path: %w", err)
	}
	target, err := pathid.Resolve(abs)
	if err != nil {
		return "", fmt.Errorf("tools: cannot resolve path %s: %w", path, err)
	}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		rootID, err := pathid.Resolve(root)
		if err != nil {
			return "", fmt.Errorf("tools: resolve sandbox root %s: %w", root, err)
		}
		if rootID.Contains(target) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrSandboxViolation, path)
}
