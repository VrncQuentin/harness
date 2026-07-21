package tools

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

const shellOutputLimit = 64 * 1024

type shellExecTool struct{}

var _ Tool = (*shellExecTool)(nil)

func (t *shellExecTool) ID() string { return "shell_exec" }
func (t *shellExecTool) Description() string {
	return "Execute a shell command. The command runs inside the sandbox root directory. Commands are limited to 30s and output is truncated to 64KB."
}
func (t *shellExecTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "The shell command to execute"},
		},
		"required": []string{"command"},
	}
}

func (t *shellExecTool) Execute(ctx context.Context, c Context, args map[string]any) Result {
	cmdStr, ok := args["command"].(string)
	if !ok || cmdStr == "" {
		return Result{Error: "shell_exec: missing or invalid command argument"}
	}
	if len(c.SandboxRoots) == 0 || c.SandboxRoots[0] == "" {
		return Result{Error: "shell_exec: no sandbox root configured — cannot determine working directory"}
	}
	workDir := c.SandboxRoots[0]

	// Validate workDir the same way file tools validate paths.
	if _, err := validatePath(workDir, c.SandboxRoots); err != nil {
		return Result{Error: fmt.Sprintf("shell_exec: invalid working directory: %v", err)}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	name, shellArgs := shellCommand(cmdStr)
	cmd := exec.CommandContext(timeoutCtx, name, shellArgs...)
	cmd.Dir = workDir
	output := newCappedOutput(shellOutputLimit)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		return Result{Error: fmt.Sprintf("shell_exec: %v\n%s", err, output.String())}
	}
	return Result{Content: output.String()}
}

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

func shellCommand(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd.exe", []string{"/d", "/s", "/c", command}
	}
	return "sh", []string{"-c", command}
}
