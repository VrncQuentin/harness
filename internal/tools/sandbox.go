package tools

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/VrncQuentin/harness/internal/pathid"
	"github.com/VrncQuentin/harness/internal/rootfs"
)

// openTarget resolves path against the call's sandbox roots and returns it as
// an open, rooted target. The caller closes it.
//
// Every tool that reads or writes file content goes through here rather than
// through validatePath. validatePath establishes where a path is and then hands
// back a string, so the access that follows resolves the same name a second
// time and may resolve it differently — the check and the operation are two
// separate walks of a mutable tree. A Target carries an open handle on the
// owning sandbox root instead, so the operation happens inside the directory
// the check approved rather than inside whatever the name means by then.
func openTarget(path string, roots []string) (*rootfs.Target, error) {
	target, err := rootfs.Set(roots).Open(path)
	if err != nil {
		// The sandbox has one refusal the tool layer names for itself; the rest
		// are resolution failures whose own wording is more informative.
		if errors.Is(err, rootfs.ErrOutsideRoots) {
			return nil, fmt.Errorf("%w: %s", ErrSandboxViolation, path)
		}
		return nil, err
	}
	return target, nil
}

// validatePath checks that path (after physical resolution) is within at
// least one sandbox root, and returns its absolute spelling. If sandbox roots
// are empty, all paths are rejected.
//
// It answers where a path is and nothing more, which is all its remaining
// callers can use. A working directory handed to a subprocess (exec, go_test,
// go_lint) and a repository path handed to go-git are both consumed as
// pathnames by something outside this package: neither a child process nor
// go-git can be given an open directory handle, so there is no rooted access to
// route them through. Tools that read or write file content themselves use
// openTarget instead.
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
