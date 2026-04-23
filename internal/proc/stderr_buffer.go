package proc

import (
	"bytes"
	"strings"
	"sync"
)

// stderrBuffer is a bounded, line-oriented ring buffer for child process
// stderr. It implements io.Writer and retains only the most recent
// maxLines complete lines so a long-running crash loop can't grow
// memory unbounded.
type stderrBuffer struct {
	mu       sync.Mutex
	maxLines int
	lines    []string
	partial  []byte
}

func newStderrBuffer(maxLines int) *stderrBuffer {
	if maxLines <= 0 {
		maxLines = 32
	}
	return &stderrBuffer{maxLines: maxLines}
}

func (b *stderrBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.partial = append(b.partial, p...)
	for {
		i := bytes.IndexByte(b.partial, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(b.partial[:i]), "\r")
		b.partial = b.partial[i+1:]
		b.appendLine(line)
	}
	return len(p), nil
}

// Snapshot returns a copy of the retained lines, including any trailing
// unterminated fragment so callers always see the latest output even if
// the process died mid-line.
func (b *stderrBuffer) Snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.lines)+1)
	out = append(out, b.lines...)
	if len(b.partial) > 0 {
		out = append(out, strings.TrimRight(string(b.partial), "\r"))
	}
	return out
}

// Reset drops all retained output. Called on every (re)start so stderr
// shown for an unhealthy child reflects only its most recent attempt.
func (b *stderrBuffer) Reset() {
	b.mu.Lock()
	b.lines = b.lines[:0]
	b.partial = b.partial[:0]
	b.mu.Unlock()
}

func (b *stderrBuffer) appendLine(line string) {
	if len(b.lines) < b.maxLines {
		b.lines = append(b.lines, line)
		return
	}
	copy(b.lines, b.lines[1:])
	b.lines[b.maxLines-1] = line
}
