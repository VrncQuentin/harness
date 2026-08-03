package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"unicode/utf8"
)

// readTool implements the read tool: range- and locator-addressed reads.
// It returns raw bytes; skeletonization is applied downstream by the
// governor (B1), never here.
type readTool struct{}

var _ Tool = (*readTool)(nil)

func (t *readTool) ID() string { return "read" }

func (t *readTool) Description() string {
	return "Read a file or a line range. Address by path (whole file), path plus start_line/end_line, a locator (path:start-end) from ast_map/ast_find, or a toolout:<id> handle to retrieve the full output of a failed tool call. Returns raw content."
}

func (t *readTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file to read. Omit when locator is given.",
			},
			"locator": map[string]any{
				"type":        "string",
				"description": "Stable locator (path:start-end) from ast_map/ast_find, or a toolout:<id> handle from a truncated tool failure. Takes precedence over path. Page a toolout handle with offset, not line numbers.",
			},
			"start_line": map[string]any{
				"type":        "integer",
				"description": "First line to read (1-based, inclusive). Requires path or a path:start-end locator; not accepted with a toolout handle.",
			},
			"end_line": map[string]any{
				"type":        "integer",
				"description": "Last line to read (1-based, inclusive). Requires start_line; not accepted with a toolout handle.",
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "Byte offset to resume a toolout handle from, taken from the continuation note of the previous page. Only for toolout locators.",
			},
		},
	}
}

func (t *readTool) Execute(ctx context.Context, c CallInfo, args map[string]any) Result {
	if locator, ok := args["locator"].(string); ok && isTooloutLocator(locator) {
		// Line addressing is refused rather than ignored. Silently treating
		// start_line/end_line as absent turned a mis-addressed read into a
		// first page, so the caller believed it had the lines it asked for.
		if _, present := args["start_line"]; present {
			return Result{Error: "read: start_line/end_line do not address a toolout handle — page it with offset instead"}
		}
		if _, present := args["end_line"]; present {
			return Result{Error: "read: start_line/end_line do not address a toolout handle — page it with offset instead"}
		}
		offset, err := integerArg(args, "offset")
		if err != nil {
			return Result{Error: "read: " + err.Error()}
		}
		return readToolout(c, locator, offset)
	}
	target, start, end, err := readTarget(args)
	if err != nil {
		return Result{Error: "read: " + err.Error()}
	}
	file, err := openTarget(target, c.SandboxRoots)
	if err != nil {
		return Result{Error: err.Error()}
	}
	defer file.Close() //nolint:errcheck // read-only root handle
	data, err := file.Read()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{Error: ErrPathNotFound.Error() + ": " + file.Display()}
		}
		return Result{Error: fmt.Sprintf("read: %v", err)}
	}
	if start == 0 {
		return Result{Content: string(data)}
	}
	span, err := spanBytes(data, start, end)
	if err != nil {
		return Result{Error: "read: " + err.Error()}
	}
	return Result{Content: string(span)}
}

// tooloutPageLimit bounds how much spilled output a single read injects.
//
// A spill can be megabytes. Returning one whole would put back into context
// exactly the volume the tee existed to keep out of it, so a read returns one
// page and says where the next one starts.
const tooloutPageLimit = 32 * 1024

// readToolout serves output the governor spilled to disk, addressed by the
// toolout:<id> handle it injected into the conversation. Without this the
// handle was a dead reference: B3 advertised a locator that no tool resolved,
// so the preserved output could not be reached at all.
//
// Paging is by byte offset rather than by line, and every response is bounded
// by construction. Line addressing cannot do this job for arbitrary captured
// output: a page boundary lands mid-line, so a line-numbered continuation
// either skips the unseen remainder of that line or re-sends the whole of it,
// and one line of a spill can be the entire file. Byte offsets have neither
// problem — consecutive pages concatenate back into exactly the spilled bytes.
func readToolout(c CallInfo, locator string, offset int) Result {
	if offset < 0 {
		return Result{Error: fmt.Sprintf("read: offset %d is negative", offset)}
	}
	data, err := openToolout(c.TooloutDir, locator)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{Error: fmt.Sprintf("read: %s no longer exists — spilled output is cached, not permanent", locator)}
		}
		return Result{Error: "read: " + err.Error()}
	}

	if offset > len(data) {
		return Result{Error: fmt.Sprintf("read: offset %d is past the end of %s (%d bytes)", offset, locator, len(data))}
	}
	// Resuming inside a character would emit invalid UTF-8 as the first bytes
	// of the page. Only offsets this tool handed out can be on a boundary by
	// construction, so a hand-made one is checked rather than trusted.
	if offset < len(data) && !utf8.RuneStart(data[offset]) {
		return Result{Error: fmt.Sprintf(
			"read: offset %d is inside a multi-byte character — resume from an offset this tool reported", offset)}
	}

	rest := data[offset:]
	// The cut lands on a rune boundary so the page is valid UTF-8. The next
	// offset is measured from the page actually returned, so no byte is
	// skipped and none is repeated.
	end := offset + runeSafeCut(string(rest), tooloutPageLimit)
	page := data[offset:end]

	if end == len(data) {
		if offset == 0 {
			return Result{Content: string(page)}
		}
		return Result{Content: fmt.Sprintf("%s\n… (bytes %d-%d of %d; end of output)",
			page, offset, end, len(data))}
	}
	return Result{Content: fmt.Sprintf(
		"%s\n… (bytes %d-%d of %d; continue with locator %s and offset %d)",
		page, offset, end, len(data), locator, end)}
}

// readTarget resolves the addressed file and optional range from the
// arguments. start of 0 means "whole file".
func readTarget(args map[string]any) (path string, start, end int, err error) {
	if locator, ok := args["locator"].(string); ok && locator != "" {
		return ParseLocator(locator)
	}
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return "", 0, 0, errors.New("missing path or locator argument")
	}
	start = intArg(args, "start_line")
	end = intArg(args, "end_line")
	if start == 0 && end == 0 {
		return path, 0, 0, nil
	}
	if start < 1 || end < start {
		return "", 0, 0, fmt.Errorf("invalid line range %d-%d", start, end)
	}
	return path, start, end, nil
}

// integerArg reads an argument that must be a whole number when present,
// returning 0 when it is absent.
//
// Nothing validates a tool call's arguments against its schema between the
// model and Execute, so a wrong type arrives here as a wrong type. intArg maps
// anything it does not recognise to 0, which for an offset silently turns a
// malformed continuation into a valid one addressing the start of the file —
// the caller gets a plausible page and no indication it asked for something
// else. A fractional number is refused for the same reason: truncating it
// invents an offset the caller did not name.
func integerArg(args map[string]any, key string) (int, error) {
	raw, present := args[key]
	if !present || raw == nil {
		return 0, nil
	}
	switch v := raw.(type) {
	case int:
		return v, nil
	case float64:
		if v != math.Trunc(v) {
			return 0, fmt.Errorf("%s must be a whole number, got %v", key, v)
		}
		if v > math.MaxInt32 || v < math.MinInt32 {
			return 0, fmt.Errorf("%s is out of range: %v", key, v)
		}
		return int(v), nil
	default:
		return 0, fmt.Errorf("%s must be a number, got %T", key, raw)
	}
}

// intArg reads an integer argument that arrives as a JSON number.
func intArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}
