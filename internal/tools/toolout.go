package tools

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/VrncQuentin/harness/internal/pathid"
)

// TooloutScheme prefixes the handles the governor's tee-on-failure emits for
// output it wrote to disk, as in "toolout:1a2b3c4d5e6f7890".
const TooloutScheme = "toolout:"

// ErrTooloutUnavailable is returned when a toolout handle cannot be resolved
// because no spill directory is configured for the call.
var ErrTooloutUnavailable = errors.New("tools: no toolout directory configured for this call")

// isTooloutLocator reports whether locator addresses spilled tool output.
func isTooloutLocator(locator string) bool {
	return strings.HasPrefix(locator, TooloutScheme)
}

// resolveToolout maps a toolout:<id> handle onto the file the governor wrote.
//
// The spill directory sits under the harness home, outside every sandbox root,
// so this deliberately does not go through validatePath — a sandbox check would
// reject every handle.
//
// Two checks stand in for it, and both are needed. The id is matched against
// the shape B3 generates — lowercase hex and nothing else — so a separator, a
// dot, a drive letter, or an absolute path is refused rather than joined onto
// the directory. That is lexical only, and a lexically perfect name still says
// nothing about where the file it names actually is: a symlink or reparse
// point called deadbeefdeadbeef inside the spill directory would be followed
// straight out of it. So the resolved leaf must also be physically inside the
// resolved directory, established through internal/pathid for the same reason
// it exists at all — filepath.EvalSymlinks cannot answer this on Windows.
func resolveToolout(dir, locator string) (string, error) {
	id, ok := strings.CutPrefix(locator, TooloutScheme)
	if !ok {
		return "", fmt.Errorf("tools: %q is not a toolout handle", locator)
	}
	if dir == "" {
		return "", ErrTooloutUnavailable
	}
	if !validTooloutID(id) {
		return "", fmt.Errorf("tools: %q is not a valid toolout id — expected lowercase hex", id)
	}

	target := filepath.Join(dir, id)
	resolvedDir, err := pathid.Resolve(dir)
	if err != nil {
		return "", fmt.Errorf("tools: cannot resolve the toolout directory: %w", err)
	}
	resolvedTarget, err := pathid.Resolve(target)
	if err != nil {
		return "", fmt.Errorf("tools: cannot resolve %s: %w", locator, err)
	}
	if !pathid.WithinRoot(resolvedTarget, resolvedDir) {
		return "", fmt.Errorf("tools: %s resolves outside the toolout directory", locator)
	}
	return target, nil
}

// tooloutIDMaxLen bounds the accepted id length. B3 emits 16 hex characters;
// the allowance leaves room for a wider digest without accepting arbitrary
// strings.
const tooloutIDMaxLen = 64

// validTooloutID accepts only non-empty lowercase hex within the length bound.
func validTooloutID(id string) bool {
	if id == "" || len(id) > tooloutIDMaxLen {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
