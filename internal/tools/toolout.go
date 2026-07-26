package tools

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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
// reject every handle. That makes the id the only thing standing between a
// crafted locator and an arbitrary file, so it is matched strictly against the
// shape B3 generates: lowercase hex and nothing else. A separator, a dot, a
// drive letter, or an absolute path is refused rather than joined onto the
// directory, so no handle can address anything outside it.
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
