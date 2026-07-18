package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Record is one line in sessions.jsonl. The schema is
// stable - existing records must keep parsing as the design grows, so
// new fields land with omitempty defaults rather than replacing old
// columns.
type Record struct {
	ID          string    `json:"id"`
	Agent       string    `json:"agent"`
	Project     string    `json:"project"`
	StartedAt   time.Time `json:"started_at"`
	SavedAt     time.Time `json:"saved_at"`
	SaveSeq     int       `json:"save_seq"`
	EpisodePath string    `json:"episode_path"`
}

// readMaxLineBytes caps a single sessions.jsonl line at 1 MiB. A line
// longer than this is almost certainly garbled data appended by a
// crashed writer; we still want the bufio.Scanner to recover instead
// of returning ErrTooLong on the whole file. The same value is used as
// both the initial allocation and the growth ceiling so the buffer
// math stays self-evidently correct.
const readMaxLineBytes = 1024 * 1024

// ReadAll parses the entire log at path. Garbled lines are skipped with
// a slog.Warn that names the line number so an operator can find the
// offending entry. The caller still gets every parseable record so a
// single bad line never blocks the harness from starting.
//
// A missing log file is not an error - it returns an empty slice. The
// log only exists once at least one session has been saved.
func ReadAll(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: read log %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, readMaxLineBytes), readMaxLineBytes)
	var out []Record
	line := 0
	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			slog.Warn("session: skipping garbled sessions.jsonl line",
				"path", path,
				"line", line,
				"err", err,
			)
			continue
		}
		if rec.ID == "" {
			slog.Warn("session: skipping log entry with empty id",
				"path", path,
				"line", line,
			)
			continue
		}
		out = append(out, rec)
	}
	if err := scanner.Err(); err != nil {
		// A scan error halfway through still returns the records we
		// already parsed - same recovery posture as a garbled line.
		slog.Warn("session: sessions.jsonl scan error",
			"path", path,
			"err", err,
		)
	}
	return out, nil
}

// AppendRecord appends rec to the log at path. The parent directory is
// created on first call so the caller does not need to scaffold the
// project repo tree separately. Each record is fsynced so a power
// loss between saves keeps the previous records intact.
func AppendRecord(path string, rec Record) error {
	if rec.ID == "" {
		return errors.New("session: append: record has empty id")
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("session: mkdir %s: %w", dir, err)
		}
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("session: marshal record: %w", err)
	}
	body = append(body, '\n')

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("session: open log %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("session: write log %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("session: sync log %s: %w", path, err)
	}
	return nil
}

// LatestPerID dedupes records to keep only the latest entry per id.
// "Latest" is the highest SaveSeq, with a SavedAt tiebreak so a
// hand-edited log that resets SaveSeq still resolves deterministically.
// The returned slice preserves the order of first-seen ids so callers
// can sort it themselves without losing the original log progression.
func LatestPerID(records []Record) []Record {
	if len(records) == 0 {
		return nil
	}
	winners := make(map[string]int, len(records))
	order := make([]string, 0, len(records))
	for i, r := range records {
		idx, ok := winners[r.ID]
		if !ok {
			winners[r.ID] = i
			order = append(order, r.ID)
			continue
		}
		// Prefer the higher save_seq; fall back to the later SavedAt.
		cur := records[idx]
		if r.SaveSeq > cur.SaveSeq ||
			(r.SaveSeq == cur.SaveSeq && r.SavedAt.After(cur.SavedAt)) {
			winners[r.ID] = i
		}
	}
	out := make([]Record, 0, len(order))
	for _, id := range order {
		out = append(out, records[winners[id]])
	}
	return out
}

// sortByNewest sorts records by SavedAt descending, ID ascending as a
// stable tie-breaker. Used by the resume picker.
func sortByNewest(records []Record) {
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].SavedAt.Equal(records[j].SavedAt) {
			return records[i].SavedAt.After(records[j].SavedAt)
		}
		return records[i].ID < records[j].ID
	})
}
