package retrieval

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// NDJSONSink is a TraceSink that writes rows to date-bucketed NDJSON files
// under logDir/retrieval/<date>.ndjson. Rotation happens on date boundary;
// files older than retainDays are deleted on each rotation.
//
// Writes are synchronous per call (no buffering) but the file is opened
// lazily and kept open across calls for the same date. Safe for concurrent use.
type NDJSONSink struct {
	logDir      string
	retainDays  int
	now         func() time.Time // injectable for tests
	mu          sync.Mutex
	currentDate string
	f           *os.File
}

// NewNDJSONSink creates a sink that appends trace rows to NDJSON files under
// logDir/retrieval/. retainDays controls how many days of logs are kept
// (older files are pruned on each date rotation). The directory is created
// lazily on first write.
func NewNDJSONSink(logDir string, retainDays int) *NDJSONSink {
	return &NDJSONSink{
		logDir:     logDir,
		retainDays: retainDays,
		now:        time.Now,
	}
}

// Emit writes one JSON-encoded trace row followed by a newline. Errors are
// logged via slog and silently dropped so a trace failure never affects the
// retrieval result.
func (s *NDJSONSink) Emit(row RetrievalTrace) {
	b, err := json.Marshal(row)
	if err != nil {
		slog.Warn("retrieval: trace marshal", "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureFile(); err != nil {
		slog.Warn("retrieval: trace file", "err", err)
		return
	}
	b = append(b, '\n')
	if _, err := s.f.Write(b); err != nil {
		slog.Warn("retrieval: trace write", "err", err)
	}
}

// Close flushes and closes the current log file. Safe to call multiple times.
func (s *NDJSONSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	s.currentDate = ""
	return err
}

// ensureFile opens or rotates the log file for today's date. Must be called with mu held.
func (s *NDJSONSink) ensureFile() error {
	today := s.now().UTC().Format("2006-01-02")
	if s.currentDate == today && s.f != nil {
		return nil
	}
	// Close previous file on date rotation.
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
		s.pruneOld(today)
	}
	dir := filepath.Join(s.logDir, "retrieval")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("retrieval: trace dir: %w", err)
	}
	path := filepath.Join(dir, today+".ndjson")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("retrieval: open trace file: %w", err)
	}
	s.f = f
	s.currentDate = today
	return nil
}

// pruneOld removes NDJSON files older than retainDays. Must be called with mu held.
func (s *NDJSONSink) pruneOld(today string) {
	if s.retainDays <= 0 {
		return
	}
	dir := filepath.Join(s.logDir, "retrieval")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := s.now().UTC().AddDate(0, 0, -s.retainDays).Format("2006-01-02")
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Only touch files matching our YYYY-MM-DD.ndjson pattern.
		if len(name) != len("2006-01-02.ndjson") || name[len(name)-7:] != ".ndjson" {
			continue
		}
		date := name[:10]
		if date < cutoff {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}
