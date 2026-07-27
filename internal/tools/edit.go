package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/VrncQuentin/harness/internal/parser"
	"github.com/VrncQuentin/harness/internal/rootfs"
)

// editTool implements the edit tool: hash-anchored line operations on
// existing files plus a whole-file mode restricted to new-file creation.
// The anchor hash comes from ast_map/ast_find output, so the model cannot
// edit a span it has not located first (recon-first, enforced by the input
// type). Verify-after-mutate is mandatory: the tool re-reads the file and
// confirms the span landed, and re-parses supported languages.
type editTool struct {
	parsers *parser.Registry
}

var _ Tool = (*editTool)(nil)

func (t *editTool) ID() string { return "edit" }

func (t *editTool) Description() string {
	return "Edit a file. Anchored mode replaces the lines of a locator (path:start-end) and requires the anchor_hash from ast_map/ast_find; the edit is rejected when the hash no longer matches. Whole-file mode (path + content, no locator) only creates new files. To insert, include the anchored lines in content. Empty content deletes the span."
}

func (t *editTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"locator": map[string]any{
				"type":        "string",
				"description": "Locator (path:start-end) of the lines to replace, from ast_map/ast_find",
			},
			"anchor_hash": map[string]any{
				"type":        "string",
				"description": "Content hash (h:…) of the locator span, from ast_map/ast_find. Required with locator.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Whole-file mode: path of a NEW file to create. Existing files must be edited via locator.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Replacement lines (anchored mode) or full file content (whole-file mode)",
			},
		},
		"required": []string{"content"},
	}
}

func (t *editTool) Execute(ctx context.Context, c CallInfo, args map[string]any) Result {
	content, ok := args["content"].(string)
	if !ok {
		return Result{Error: "edit: missing content argument"}
	}
	if locator, hasLoc := args["locator"].(string); hasLoc && locator != "" {
		anchor, _ := args["anchor_hash"].(string)
		return t.editAnchored(c, locator, anchor, content)
	}
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return Result{Error: "edit: need either locator+anchor_hash or path"}
	}
	return t.createFile(c, path, content)
}

func (t *editTool) editAnchored(c CallInfo, locator, anchor, content string) Result {
	path, start, end, err := ParseLocator(locator)
	if err != nil {
		return Result{Error: "edit: " + err.Error()}
	}
	if anchor == "" {
		return Result{Error: "edit: anchor_hash is required — take it from ast_map/ast_find output"}
	}
	file, err := openTarget(path, c.SandboxRoots)
	if err != nil {
		return Result{Error: err.Error()}
	}
	defer file.Close() //nolint:errcheck // root handle, no buffered writes
	src, err := file.Read()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{Error: ErrPathNotFound.Error() + ": " + file.Display()}
		}
		return Result{Error: fmt.Sprintf("edit: %v", err)}
	}
	currentHash, err := SpanHash(src, start, end)
	if err != nil {
		return Result{Error: "edit: " + err.Error()}
	}
	if currentHash != anchor {
		return Result{Error: fmt.Sprintf(
			"edit: anchor hash mismatch for %s (expected %s, span is now %s) — the file changed since it was located; re-run ast_find and retry",
			locator, anchor, currentHash)}
	}

	updated, replacement := spliceSpan(src, start, end, content)
	if err := file.WriteAtomic(updated, editFileMode); err != nil {
		return Result{Error: fmt.Sprintf("edit: %v", err)}
	}
	return t.verifyAfterMutate(file, start, replacement, "edited "+FormatLocator(file.Display(), start, end))
}

func (t *editTool) createFile(c CallInfo, path, content string) Result {
	file, err := openTarget(path, c.SandboxRoots)
	if err != nil {
		return Result{Error: err.Error()}
	}
	defer file.Close() //nolint:errcheck // root handle, no buffered writes
	// The parents are created through the same pinned root as the write. The
	// previous version created them by pathname and then re-ran the sandbox
	// check, because a MkdirAll that followed a link could otherwise have moved
	// the file it was about to write outside the sandbox. Inside the root there
	// is nowhere for it to go, so the second check has nothing left to catch.
	if err := file.MkdirAllParent(0o755); err != nil {
		return Result{Error: fmt.Sprintf("edit: create parent directories: %v", err)}
	}
	// "Only creates new files" is enforced by the create itself, not by a Stat
	// before it. Checking first and then publishing by rename is a check/use
	// race: a file that appears in between is silently replaced by the rename,
	// and the caller is told a new file was created. O_EXCL makes the existence
	// check and the claim on the name one operation, so the loser of a race
	// gets an error rather than the winner losing their content.
	if err := file.CreateExclusive([]byte(content), editFileMode); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Result{Error: fmt.Sprintf("edit: %s already exists — whole-file mode only creates new files; use ast_find + anchored edit", file.Display())}
		}
		return Result{Error: fmt.Sprintf("edit: %v", err)}
	}
	return t.verifyAfterMutate(file, 1, []byte(content), "created "+file.Display())
}

// editFileMode is the permission an edited or created file is left with.
const editFileMode = 0o644

// verifyAfterMutate re-reads the file, confirms the written span matches the
// requested bytes, and re-parses supported languages. It is not optional.
//
// The re-read goes back through the same pinned root as the write, so what is
// verified is the file that was written rather than whatever the pathname
// resolves to by the time the check runs.
func (t *editTool) verifyAfterMutate(file *rootfs.Target, start int, replacement []byte, action string) Result {
	absPath := file.Display()
	after, err := file.Read()
	if err != nil {
		return Result{Error: fmt.Sprintf("edit: verify re-read: %v", err)}
	}

	summary := action
	if len(replacement) == 0 {
		summary += " (span deleted)"
	} else {
		newEnd := start + countLines(replacement) - 1
		span, err := spanBytes(after, start, newEnd)
		if err != nil {
			return Result{Error: fmt.Sprintf("edit: verify: %v", err)}
		}
		if string(span) != string(replacement) {
			return Result{Error: fmt.Sprintf("edit: verify failed — %s does not contain the written content; the file may have been modified concurrently", absPath)}
		}
		hash, err := SpanHash(after, start, newEnd)
		if err != nil {
			return Result{Error: fmt.Sprintf("edit: verify: %v", err)}
		}
		summary += fmt.Sprintf(" → %s %s", FormatLocator(absPath, start, newEnd), hash)
	}

	if front, ok := t.parsers.ForPath(absPath); ok {
		if err := front.Check(after); err != nil {
			return Result{Content: summary + fmt.Sprintf("\nverify: content OK; WARNING: file no longer parses: %v", err)}
		}
		return Result{Content: summary + "\nverify: content OK; parse OK"}
	}
	return Result{Content: summary + "\nverify: content OK"}
}

// spliceSpan replaces lines start..end of src with content and returns the
// updated file plus the exact replacement bytes written. A non-empty
// replacement that does not end in a newline gains one when lines follow it,
// so the next line is never fused onto the replacement.
func spliceSpan(src []byte, start, end int, content string) (updated, replacement []byte) {
	lines := splitLinesKeepEnds(src)
	replacement = []byte(content)
	if len(replacement) > 0 && end < len(lines) && !strings.HasSuffix(content, "\n") {
		replacement = append(replacement, '\n')
	}
	var out []byte
	for _, line := range lines[:start-1] {
		out = append(out, line...)
	}
	out = append(out, replacement...)
	for _, line := range lines[end:] {
		out = append(out, line...)
	}
	return out, replacement
}

// countLines counts the lines in b, where a trailing newline does not open
// a new line.
func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}
