package tools

import (
	"errors"
	"fmt"
	"os"
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

// resolveToolout validates a toolout:<id> handle and returns the bare id, for
// opening relative to the spill directory handle.
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
// it. openToolout supplies the physical half by resolving the id through an
// open handle on the directory, and callers must use it rather than joining
// this id onto a path themselves.
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
	return id, nil
}

// openToolout opens the file a toolout handle addresses, resolving it relative
// to an open handle on the spill directory rather than by pathname.
//
// os.Root holds the directory open and resolves each component against that
// handle, so containment is an ancestry relationship rather than a comparison
// of strings. Two earlier attempts were both weaker than they looked:
//
//   - Canonicalizing the target's path and reopening it by name checks one
//     resolution and reads another, so whatever the name meant during the check
//     can be replaced before the open.
//   - Canonicalizing an open target against a pinned root path fixes the target
//     but still compares pathnames. A pathname is not an identity: rename the
//     real directory aside, move an attacker's directory into the name it
//     vacated, and the target opens inside the attacker's directory while
//     canonicalizing to a path that sits under the pinned string. The
//     comparison agrees with itself and admits the file.
//
// Resolving through the handle removes the pathname from the decision
// altogether. It also refuses symlinked and junctioned leaves, and any
// traversal, without a separate check.
//
// The rest of the tool layer still resolves and reopens by path; this is the
// one place that reads outside every sandbox root, which is why it does not.
func openToolout(dir, locator string) (*os.File, error) {
	id, err := resolveToolout(dir, locator)
	if err != nil {
		return nil, err
	}

	// %v rather than %w keeps a missing spill directory from being reported as
	// a missing spill file.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("tools: toolout directory unavailable: %v", err)
	}
	defer root.Close() //nolint:errcheck // read-only handle

	if tooloutSwapHook != nil {
		tooloutSwapHook()
	}

	f, err := root.Open(id)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// tooloutSwapHook runs between pinning the spill root and opening the target.
// It is nil in production.
//
// It exists because the ordering it guards is only observable when the spill
// directory is replaced in exactly that window, which cannot be staged from
// outside the function. Without it the ordering could be argued from reading
// the code but not demonstrated, and an argument is what the previous version
// of this comment offered while the code did something weaker.
var tooloutSwapHook func()

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
