package tools

import (
	"errors"
	"fmt"
	"os"
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
// This is the lexical half of the check: the id is matched against the shape
// B3 generates — lowercase hex and nothing else — so a separator, a dot, a
// drive letter, or an absolute path is refused rather than joined onto the
// directory.
//
// It is not sufficient on its own. A lexically perfect name says nothing about
// where the file it names actually is: a symlink or reparse point called
// deadbeefdeadbeef inside the spill directory would be followed straight out of
// it. openToolout supplies the physical half, and callers must use it rather
// than opening this path themselves.
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

	return filepath.Join(dir, id), nil
}

// openToolout opens the file a toolout handle addresses and returns it only if
// the opened handle is physically inside the spill directory.
//
// Containment is established from the handle rather than from the path, and the
// caller reads from that same handle. Canonicalizing a path and reopening it by
// name afterwards checks one resolution and reads another: whatever the name
// referred to during the check can be replaced before the open, and the read
// follows the replacement. Validating the handle closes that window — the file
// that was checked and the file that is read are the same object.
//
// The rest of the tool layer still resolves and reopens by path; this is the
// one place that reads outside every sandbox root, which is why it does not.
func openToolout(dir, locator string) (*os.File, error) {
	path, err := resolveToolout(dir, locator)
	if err != nil {
		return nil, err
	}
	//nolint:gosec // dir joined with an id validated as bare lowercase hex
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	resolvedDir, err := pathid.Resolve(dir)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("tools: cannot resolve the toolout directory: %w", err)
	}
	opened, err := pathid.CanonicalFile(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("tools: cannot identify %s: %w", locator, err)
	}
	if !pathid.WithinRoot(opened, resolvedDir) {
		_ = f.Close()
		return nil, fmt.Errorf("tools: %s resolves outside the toolout directory", locator)
	}
	return f, nil
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
