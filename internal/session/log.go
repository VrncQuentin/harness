package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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

// LogReader is the rooted read capability the session log needs. relPath is
// relative to the project memory repo, which the implementation holds open.
type LogReader interface {
	Read(relPath string) ([]byte, error)
}

// LogAppender is the rooted append capability the session log needs. It is
// deliberately narrower than a file API: an append-only audit log must not be
// reachable through anything that can truncate or replace it.
type LogAppender interface {
	AppendFile(relPath string, data []byte) error
}

// ReadAll parses the entire log at relPath, read through the pinned project
// memory repo rather than by pathname. Garbled lines are skipped with
// a slog.Warn that names the line number so an operator can find the
// offending entry. The caller still gets every parseable record so a
// single bad line never blocks the harness from starting.
//
// A missing log file is not an error - it returns an empty slice. The
// log only exists once at least one session has been saved.
func ReadAll(r LogReader, relPath string) ([]Record, error) {
	data, err := r.Read(relPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: read log %s: %w", relPath, err)
	}
	path := relPath

	scanner := bufio.NewScanner(bytes.NewReader(data))
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

// AppendRecord appends rec to the log at relPath, through the pinned project
// memory repo. The parent directory is created on first call so the caller does
// not need to scaffold the project repo tree separately. Each record is fsynced
// so a power loss between saves keeps the previous records intact.
//
// The append goes through a rooted capability rather than an absolute pathname
// for the same reason the log is append-only in the first place: a name that
// resolves somewhere else — because a component of it became a link, or because
// the repo directory was replaced — would send the harness's own audit trail
// out of the repository, or let a write land on a file that is not the log.
func AppendRecord(w LogAppender, relPath string, rec Record) error {
	if rec.ID == "" {
		return errors.New("session: append: record has empty id")
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("session: marshal record: %w", err)
	}
	body = append(body, '\n')

	if err := w.AppendFile(relPath, body); err != nil {
		return fmt.Errorf("session: write log %s: %w", relPath, err)
	}
	return nil
}

// LatestPerID dedupes records to keep only the latest entry per id.
// "Latest" is the highest SaveSeq, with a SavedAt tiebreak so a
// hand-edited log that resets SaveSeq still resolves deterministically,
// and a same-SaveSeq-and-SavedAt tie falling back to physical log position
// (the later record, by iteration order, wins) so two records that are
// otherwise indistinguishable still resolve to whichever one sessions.jsonl's
// own last-wins-by-ID contract actually means: the one appended later. This
// last case is not just theoretical — session.Manager.Save appends a
// provisional record right after the sidecar write (EpisodePath empty) and,
// on success, a final record for the same save carrying the identical
// SaveSeq and SavedAt on purpose (see Save's own comment on why: both
// describe the same logical save attempt). Comparing only SaveSeq and a
// strict "after" on SavedAt left the provisional record as the winner in
// that exact case, since the final record is never strictly later — Records
// (and anything built on it, like the resume picker) would keep surfacing
// the empty EpisodePath even after a save completed successfully.
// findLatestRecord (used by Resume) does not share this bug: it just walks
// the log and keeps the last match by position, with no SaveSeq/SavedAt
// comparison to get out of sync with the log's own order.
//
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
		// Prefer the higher save_seq; fall back to the later-or-equal
		// SavedAt (not strictly later — see the tie case in the doc comment
		// above) so a later record in iteration order always wins a tie.
		cur := records[idx]
		if r.SaveSeq > cur.SaveSeq ||
			(r.SaveSeq == cur.SaveSeq && !r.SavedAt.Before(cur.SavedAt)) {
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
