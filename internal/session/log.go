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

// RecordState is the explicit recovery state written by a save attempt. It is
// typed so malformed states cannot be expressed accidentally; every production
// append writes one of the two constants.
type RecordState string

// Record states. A save attempt publishes an explicit state rather than
// leaving recovery correctness to be inferred from timestamps, empty paths,
// or physical log order.
const (
	// StatePending marks a save attempt whose raw conversation sidecar is
	// durable and whose recovery state has been published, but whose episode
	// has not yet been summarized, published, and committed. A pending session
	// is discoverable and resumable from its sidecar.
	StatePending RecordState = "pending"
	// StateComplete marks a save attempt whose episode was published and
	// committed after the sidecar and pending record for the same attempt.
	// For one attempt, complete deterministically supersedes pending.
	StateComplete RecordState = "complete"
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
	// Attempt is the monotonic save-attempt identifier allocated before any
	// fallible save work. It counts failed attempts too, so a retry never
	// reuses a number. Recovery selects records by Attempt plus State;
	// wall-clock timestamps, EpisodePath, and physical log order never decide
	// correctness. Absent on legacy records (see effectiveAttempt).
	Attempt int `json:"attempt,omitempty"`
	// State is the explicit recovery state (StatePending or StateComplete).
	// Its absence marks a legacy pre-PR-11 record, which is normalized as
	// complete by the documented legacy rule in effectiveAttempt/effectiveState.
	State RecordState `json:"state,omitempty"`
}

// readMaxLineBytes caps a single sessions.jsonl line at 1 MiB. A line
// longer than this is almost certainly garbled data appended by a
// crashed writer; we still want the bufio.Scanner to recover instead
// of returning ErrTooLong on the whole file. The same value is used as
// both the initial allocation and the growth ceiling so the buffer
// math stays self-evidently correct.
const readMaxLineBytes = 1024 * 1024

// LogReader is the rooted read capability the session log needs. relPath is
// relative to the project memory repo, which the implementation holds open —
// the manager never opens sessions.jsonl by pathname.
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
// A missing log file is not an error — it returns an empty slice. The
// log only exists once at least one session has been saved. Only
// fs.ErrNotExist means "no sessions": a permission, containment, or I/O
// failure is an error the caller must see, because treating a log it
// could not read as empty would hide a real problem.
func ReadAll(r LogReader, relPath string) ([]Record, error) {
	data, err := r.Read(relPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: read log %s: %w", relPath, err)
	}

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
				"path", relPath,
				"line", line,
				"err", err,
			)
			continue
		}
		if rec.ID == "" {
			slog.Warn("session: skipping log entry with empty id",
				"path", relPath,
				"line", line,
			)
			continue
		}
		if !validRecord(rec) {
			slog.Warn("session: skipping malformed recovery record",
				"path", relPath,
				"line", line,
				"id", rec.ID,
				"state", rec.State,
				"attempt", rec.Attempt,
				"save_seq", rec.SaveSeq,
			)
			continue
		}
		out = append(out, rec)
	}
	if err := scanner.Err(); err != nil {
		// A scan error halfway through still returns the records we
		// already parsed - same recovery posture as a garbled line.
		slog.Warn("session: sessions.jsonl scan error",
			"path", relPath,
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
//
// Unlike reading, appending accepts only explicit records: a recognized typed
// state (pending or complete) with a positive attempt. Legacy state-less
// records are only ever read, never produced — current writers must always
// publish the explicit recovery state.
func AppendRecord(w LogAppender, relPath string, rec Record) error {
	if rec.ID == "" {
		return errors.New("session: append: record has empty id")
	}
	if !validAppendRecord(rec) {
		return fmt.Errorf("session: append: malformed record (state %q, attempt %d, save_seq %d)",
			rec.State, rec.Attempt, rec.SaveSeq)
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

// validRecord reports whether r is acceptable for reading and recovery
// selection: a fully legacy record (no state or attempt fields) or a valid
// explicit record (a recognized typed state with a positive attempt). Anything
// else is a malformed hybrid — an unknown state, a state without an attempt,
// an attempt without a state, or a negative counter — and must never influence
// recovery selection. A legacy record is one that predates the explicit fields
// entirely, so it must carry no attempt field; an explicit record is only
// meaningful with a positive attempt.
func validRecord(r Record) bool {
	if r.SaveSeq < 0 || r.Attempt < 0 {
		return false
	}
	switch r.State {
	case "":
		return r.Attempt == 0
	case StatePending, StateComplete:
		return r.Attempt > 0
	default:
		return false
	}
}

// validAppendRecord is the append-time validator. It is deliberately stricter
// than validRecord: a record written by current code must always carry a
// recognized typed state and a positive attempt, so a state-less record — the
// exact shape the explicit-state format removes — can never be appended.
// Legacy records are only ever read from existing logs, never produced.
func validAppendRecord(r Record) bool {
	if r.SaveSeq < 0 || r.Attempt <= 0 {
		return false
	}
	switch r.State {
	case StatePending, StateComplete:
		return true
	default:
		return false
	}
}

// effectiveAttempt returns the monotonic attempt key used for recovery
// selection. New-format records carry an explicit Attempt; legacy records —
// those without a state field — predate the explicit-state format and were
// only ever appended after a fully successful save, so they order by save_seq.
// Only records that pass validRecord reach this function; the logs themselves
// are never rewritten, this rule only interprets records that are already on
// disk.
func effectiveAttempt(r Record) int {
	if r.State == "" {
		return r.SaveSeq
	}
	return r.Attempt
}

// effectiveState returns the normalized recovery state of a record. The
// documented legacy normalization rule: a record with no state field was
// appended only after the full save (summarize, publish, commit) succeeded,
// so it is complete. New-format state is never inferred from EpisodePath.
func effectiveState(r Record) RecordState {
	if r.State == "" {
		return StateComplete
	}
	return r.State
}

// supersedes reports whether r wins over cur for the same session id.
// Selection is by the explicit monotonic attempt identifier first, then state
// precedence: for one attempt, complete deterministically supersedes pending.
// SavedAt is never consulted — it is a display value. A same-attempt,
// same-state tie (only reachable in a hand-edited or corrupt log) resolves
// deterministically by higher SaveSeq, then first-seen in log order.
func supersedes(cur, r Record) bool {
	if effectiveAttempt(r) != effectiveAttempt(cur) {
		return effectiveAttempt(r) > effectiveAttempt(cur)
	}
	if effectiveState(r) != effectiveState(cur) {
		return effectiveState(r) == StateComplete
	}
	return r.SaveSeq > cur.SaveSeq
}

// LatestPerID dedupes records to keep only the winning entry per id.
// The winner is selected by explicit recovery state — highest effective
// attempt, with complete superseding pending for the same attempt — never by
// wall-clock timestamp, EpisodePath, or physical log order (see supersedes).
// Malformed records are skipped so they can never supersede valid history;
// callers that read the log get them pre-filtered by ReadAll, and this check
// keeps hand-built slices safe too. The returned slice preserves the order of
// first-seen ids so callers can sort it themselves without losing the original
// log progression.
func LatestPerID(records []Record) []Record {
	if len(records) == 0 {
		return nil
	}
	winners := make(map[string]int, len(records))
	order := make([]string, 0, len(records))
	for i, r := range records {
		if !validRecord(r) {
			continue
		}
		idx, ok := winners[r.ID]
		if !ok {
			winners[r.ID] = i
			order = append(order, r.ID)
			continue
		}
		if supersedes(records[idx], r) {
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
