package governor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"

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
	a := g.spillAnchor()
	if a == nil {
		return res
	}
	defer func() { _ = a.Close() }()

	id := tooloutID(toolID, spill)
	r, err := a.Open()
	if err != nil {
		return res
	}
	defer func() { _ = r.Close() }()
	if err := r.WriteStreamAtomic(id, bytes.NewReader([]byte(spill)), 0o644); err != nil {
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

// tooloutID returns a deterministic file ID for (toolID, content).
func tooloutID(toolID, content string) string {
	h := sha256.Sum256([]byte(toolID + "\x00" + content))
	return fmt.Sprintf("%x", h[:8])
}
