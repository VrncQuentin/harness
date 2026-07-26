package tools

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const execOutputLimit = 64 * 1024

// spillCeiling bounds how much subprocess output is held for the governor's
// tee-on-failure. It is far above the inline caps so the preserved copy is
// genuinely the full output in every ordinary case, but it is still a bound: a
// runaway process must not be able to exhaust memory purely because its output
// is being kept for the spill file.
const spillCeiling = 8 << 20 // 8 MiB

// boundInline returns at most limit bytes of s, appending an elision note when
// it had to cut.
//
// The cut lands on a rune boundary. Slicing raw bytes at a fixed offset splits
// multi-byte characters, and the resulting invalid UTF-8 goes straight into the
// conversation and from there into session records.
func boundInline(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… (truncated; full output preserved for retrieval)"
}

type execTool struct{}

var _ Tool = (*execTool)(nil)

func (t *execTool) ID() string { return "exec" }
func (t *execTool) Description() string {
	return "Execute a command by argv array (no shell). The command runs inside the sandbox root directory. Limited to 30s; output truncated to 64KB. Known-destructive executables are blocked."
}
func (t *execTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cmd": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Command and arguments as an argv array. The first element is the executable; no shell interpretation is applied.",
			},
		},
		"required": []string{"cmd"},
	}
}

// denyExecutables is the set of executable base-names the exec tool refuses to
// run regardless of other approval decisions. Defence-in-depth only — the human
// approval layer remains the security boundary.
var denyExecutables = map[string]bool{
	"dd":       true,
	"mkfs":     true,
	"fdisk":    true,
	"parted":   true,
	"format":   true, // Windows disk format
	"diskpart": true, // Windows disk management
	"shutdown": true,
	"reboot":   true,
	"halt":     true,
	"poweroff": true,
}

// recursiveDeleteArgs are argv flags that turn rm/del/rd into recursive deletion.
var recursiveDeleteArgs = map[string]bool{
	"-r": true, "-R": true, "-rf": true, "-fr": true, "-Rf": true,
	"--recursive": true, "/s": true,
}

func isDenied(argv []string) (bool, string) {
	base := strings.ToLower(filepath.Base(argv[0]))
	if denyExecutables[base] {
		return true, fmt.Sprintf("exec: denied: %q is a blocked executable", argv[0])
	}
	if base == "rm" || base == "del" || base == "rd" || base == "rmdir" {
		for _, arg := range argv[1:] {
			if recursiveDeleteArgs[arg] {
				return true, fmt.Sprintf("exec: denied: recursive deletion via %q is not allowed", argv[0])
			}
		}
	}
	return false, ""
}

func (t *execTool) Execute(ctx context.Context, c CallInfo, args map[string]any) Result {
	rawCmd, ok := args["cmd"]
	if !ok {
		return Result{Error: "exec: missing cmd argument"}
	}
	argv, err := parseStringSlice(rawCmd)
	if err != nil || len(argv) == 0 {
		return Result{Error: "exec: cmd must be a non-empty array of strings"}
	}

	if denied, reason := isDenied(argv); denied {
		return Result{Error: reason}
	}

	if len(c.SandboxRoots) == 0 || c.SandboxRoots[0] == "" {
		return Result{Error: "exec: no sandbox root configured — cannot determine working directory"}
	}
	workDir := c.SandboxRoots[0]
	if _, err := validatePath(workDir, c.SandboxRoots); err != nil {
		return Result{Error: fmt.Sprintf("exec: invalid working directory: %v", err)}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, argv[0], argv[1:]...) //nolint:gosec
	cmd.Dir = workDir
	output := newCappedOutput(spillCeiling)
	cmd.Stdout = output
	cmd.Stderr = output
	runErr := cmd.Run()

	full := output.String()
	inline := boundInline(full, execOutputLimit)
	if runErr != nil {
		res := Result{Error: fmt.Sprintf("exec: %v\n%s", runErr, inline)}
		// Only worth carrying when the inline text is not already the whole
		// output; the governor spills this instead of the truncated excerpt.
		if inline != full {
			res.FullOutput = full
		}
		return res
	}
	return Result{Content: inline}
}

// parseStringSlice converts an any value to []string, accepting both
// []any (from JSON decode) and []string.
func parseStringSlice(v any) ([]string, error) {
	switch tv := v.(type) {
	case []string:
		return tv, nil
	case []any:
		out := make([]string, 0, len(tv))
		for _, item := range tv {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("cmd element is not a string")
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("cmd must be an array")
	}
}

// cappedOutput is a bounded io.Writer that truncates after limit bytes while
// allowing the underlying command to complete normally.
type cappedOutput struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

func newCappedOutput(limit int) *cappedOutput {
	return &cappedOutput{limit: limit}
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.limit <= 0 {
		b.truncated = b.truncated || len(p) > 0
		return len(p), nil
	}
	remaining := b.limit - len(b.buf)
	if remaining > 0 {
		if len(p) <= remaining {
			b.buf = append(b.buf, p...)
		} else {
			b.buf = append(b.buf, p[:remaining]...)
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *cappedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := string(b.buf)
	if b.truncated {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += "... (output truncated)"
	}
	return out
}
