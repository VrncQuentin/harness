package governor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/VrncQuentin/harness/internal/rootfs"
	"github.com/VrncQuentin/harness/internal/tools"
)

// b3Threshold is the minimum error length (bytes) that triggers B3 spill.
// Short errors are injected verbatim; only large outputs (e.g. exec stderr)
// need to be teed to disk.
const b3Threshold = 4096

// applyB3 spills large error outputs to disk and replaces the inline error
// with a compact summary + retrieval handle (toolout:<id>).
// The transform degrades gracefully: if the cache dir is unavailable the
// error is returned unchanged.
func (g *Governor) applyB3(_ context.Context, toolID string, res tools.Result) tools.Result {
	if res.Error == "" {
		return res
	}
	// Spill the complete output when the tool preserved one. Writing res.Error
	// alone wrote whatever survived the tool's inline cap, so the file B3
	// advertised as the full output was in fact the same truncated text the
	// model had already been shown.
	spill := res.FullOutput
	if spill == "" {
		spill = res.Error
	}
	if len(spill) < b3Threshold {
		return res
	}
	dir := g.tooloutDir()
	if dir == "" {
		return res
	}

	id := tooloutID(toolID, spill)
	if err := writeSpill(dir, id, spill); err != nil {
		// Write failure — return unchanged.
		return res
	}

	// Keep a prefix of the error for immediate context, add the handle. The
	// scheme comes from the tool layer that resolves it, so the emitting and
	// resolving sides cannot drift apart.
	const prefixLen = 512
	res.Error = fmt.Sprintf("%s\n… (full output in %s%s)",
		res.Error[:runeSafeCutEnd(res.Error, prefixLen)], tools.TooloutScheme, id)
	// The spill is on disk and addressable now, so drop the in-memory copy
	// rather than carrying megabytes of output onward into events and session
	// records.
	res.FullOutput = ""
	return res
}

// writeSpill publishes content under id inside the spill directory.
//
// The directory is pinned and the id is resolved through that handle, so an
// entry already sitting in the spill directory under that name — a symlink, a
// junction — cannot send the write somewhere else. Publication is by rename, so
// a pre-existing entry is *replaced* rather than written through: opening the
// name and truncating would follow such a link and empty whatever it points at,
// and would also let a reader see a half-written spill while the tee is still
// copying megabytes of output into it.
//
// The pin is per-spill rather than held for the governor's lifetime. Spills are
// rare — only failures above the threshold reach here — and the directory is a
// cache the user may clear between calls, so a handle kept open would pin a
// deleted directory and quietly write into nothing.
func writeSpill(dir, id, content string) error {
	root, err := rootfs.Open(dir)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // failure to close a spill root loses nothing
	return root.WriteStreamAtomic(id, bytes.NewReader([]byte(content)), 0o644)
}

// tooloutID returns a deterministic file ID for (toolID, content).
func tooloutID(toolID, content string) string {
	h := sha256.Sum256([]byte(toolID + "\x00" + content))
	return fmt.Sprintf("%x", h[:8])
}
