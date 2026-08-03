package session

// Explicit session recovery state. These tests pin the transaction design: a
// save allocates a monotonic attempt, durably publishes the raw sidecar,
// appends an explicit pending record, summarizes, publishes and commits the
// episode, then appends an explicit complete record for the same attempt.
// Recovery selects records by the attempt identifier and state precedence,
// never by wall-clock timestamps, EpisodePath, or physical log order.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VrncQuentin/harness/internal/git"
	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/project"
)

// scriptedWriter records the artifact writes Save issues and can fail the nth
// call, so failure boundaries can be staged without touching the filesystem.
type scriptedWriter struct {
	real  FileWriter
	calls []string
	fail  int
	err   error
}

func (w *scriptedWriter) WriteFile(relPath string, data []byte) error {
	w.calls = append(w.calls, relPath)
	if w.fail > 0 && len(w.calls) == w.fail {
		return w.err
	}
	return w.real.WriteFile(relPath, data)
}

// scriptedAppender records the log appends Save issues and can fail the nth
// call (the pending or the complete append).
type scriptedAppender struct {
	real  LogAppender
	calls []string
	fail  int
	err   error
}

func (a *scriptedAppender) AppendFile(relPath string, data []byte) error {
	a.calls = append(a.calls, relPath)
	if a.fail > 0 && len(a.calls) == a.fail {
		return a.err
	}
	return a.real.AppendFile(relPath, data)
}

// scriptedCommitter records commit invocations and can fail them all.
type scriptedCommitter struct {
	real  Committer
	calls int
	fail  bool
	err   error
}

func (c *scriptedCommitter) Commit(msg string, files []string) (string, error) {
	c.calls++
	if c.fail {
		return "", c.err
	}
	return c.real.Commit(msg, files)
}

// sidecarProbeAppender verifies, inside each log append, that the raw sidecar
// is already readable before the append is allowed to land. Save appends
// pending before running summarization, so a reorder that wrote pending ahead
// of the sidecar would make the first append read a missing sidecar and fail
// the save — this proves sidecar-before-pending ordering at the actual append
// boundary rather than after Save returns.
type sidecarProbeAppender struct {
	real    LogAppender
	reader  FileReader
	sidecar string
	probes  int
}

func (a *sidecarProbeAppender) AppendFile(relPath string, data []byte) error {
	body, err := a.reader.Read(a.sidecar)
	if err != nil {
		return fmt.Errorf("sidecar not readable at append boundary: %w", err)
	}
	if len(body) == 0 {
		return errors.New("sidecar empty at append boundary")
	}
	a.probes++
	return a.real.AppendFile(relPath, data)
}

// newRecoveryRepo scaffolds a memory repo and returns its rooted reader plus
// the opened git repo.
func newRecoveryRepo(t *testing.T) (*memory.DirReader, *git.Repo) {
	t.Helper()
	root, repo := scaffoldMemoryRepo(t, "coder")
	reader, err := memory.NewDirReader(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return reader, repo
}

// recoveryManager builds a Manager wired to the given doubles; nil entries fall
// back to the rooted reader / git repo.
func recoveryManager(t *testing.T, fi *fakeInference, reader *memory.DirReader, repo *git.Repo, writer FileWriter, appender LogAppender, committer Committer) *Manager {
	t.Helper()
	if writer == nil {
		writer = reader
	}
	if appender == nil {
		appender = reader
	}
	if committer == nil {
		committer = repo
	}
	mgr, err := NewManager(ManagerDeps{
		Repo:             committer,
		Writer:           writer,
		Reader:           reader,
		Appender:         appender,
		Inference:        fi,
		SummarizerPrompt: func() string { return "test" },
	}, project.GlobalSlug)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr
}

// startSession starts a coder session and appends two messages.
func startSession(t *testing.T, mgr *Manager) *Session {
	t.Helper()
	s := mgr.Start("coder")
	if err := mgr.Append(s.ID, inference.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatalf("Append user: %v", err)
	}
	if err := mgr.Append(s.ID, inference.Message{Role: "assistant", Content: "world"}); err != nil {
		t.Fatalf("Append assistant: %v", err)
	}
	return s
}

// TestSessionRecovery_ExplicitRecordState: session state must
// be decided by the explicit record state, never inferred from wall-clock
// timestamps, an empty EpisodePath, or physical log order. Here the complete
// record has an empty EpisodePath and an earlier SavedAt while the pending
// record carries a path, the later timestamp, and is last in the log — every
// heuristic points at pending, yet the explicit state rule must select complete
// for the same attempt.
func TestSessionRecovery_ExplicitRecordState(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	records := []Record{
		{ID: "s1", Agent: "coder", SaveSeq: 1, Attempt: 3, State: StateComplete, EpisodePath: "", SavedAt: now.Add(-time.Hour)},
		{ID: "s1", Agent: "coder", SaveSeq: 0, Attempt: 3, State: StatePending, EpisodePath: "episodes/coder/s1.md", SavedAt: now},
	}
	got := LatestPerID(records)
	if len(got) != 1 {
		t.Fatalf("expected 1 winning record, got %d", len(got))
	}
	if got[0].State != StateComplete {
		t.Fatalf("winner state = %q, want complete (explicit state, not path/timestamp/order)", got[0].State)
	}
	if got[0].EpisodePath != "" {
		t.Fatalf("winner episode_path = %q, want empty — complete must hold despite a missing path", got[0].EpisodePath)
	}
}

// TestSessionRecovery_PendingAfterSidecarDurable: a pending
// state must only become visible once the raw sidecar is durable, so a session
// found pending is always resumable from its sidecar. Save publishes the
// sidecar before appending pending. The probe appender proves the ordering at
// the pending append boundary itself — the sidecar must already be readable
// before the append is allowed to land — so reordering production to append
// pending before writing the sidecar would fail this test.
func TestSessionRecovery_PendingAfterSidecarDurable(t *testing.T) {
	fi := newFakeInference([]inference.Token{{Err: errors.New("summarizer down")}})
	reader, repo := newRecoveryRepo(t)
	probe := &sidecarProbeAppender{real: reader, reader: reader}
	mgr := recoveryManager(t, fi, reader, repo, nil, probe, nil)
	s := startSession(t, mgr)
	probe.sidecar = "episodes/coder/" + s.ID + ".json"

	if _, err := mgr.Save(context.Background(), s.ID); err == nil {
		t.Fatal("save with a failing summarizer must fail")
	}

	// The probe runs inside the append: had the pending record been appended
	// before the sidecar write, the append would have read a missing sidecar
	// and failed the save here.
	if probe.probes == 0 {
		t.Fatal("the append boundary probe never ran")
	}

	records, err := ReadAll(reader, sessionsLogRel)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(records) != 1 || records[0].State != StatePending {
		t.Fatalf("log after summarizer failure = %+v, want a single pending record", records)
	}

	// At the moment the pending record is visible the sidecar is readable.
	body, err := reader.Read("episodes/coder/" + s.ID + ".json")
	if err != nil {
		t.Fatalf("sidecar not readable while the pending record is visible: %v", err)
	}
	conv, err := decodeConversation(body)
	if err != nil {
		t.Fatalf("decode sidecar: %v", err)
	}
	if len(conv) != 2 {
		t.Fatalf("sidecar conversation = %d messages, want 2", len(conv))
	}

	// The pending session resumes from that sidecar.
	got, err := mgr.Resume(s.ID)
	if err != nil {
		t.Fatalf("Resume pending session: %v", err)
	}
	if len(got.Conversation) != 2 {
		t.Fatalf("resumed conversation = %d messages, want 2", len(got.Conversation))
	}
}

// TestSessionRecovery_CompleteAfterCommit: a complete record
// must only be emitted after the episode is published and committed. A commit
// failure leaves only pending; a successful save emits complete only after the
// sidecar and episode writes and the commit all ran.
func TestSessionRecovery_CompleteAfterCommit(t *testing.T) {
	t.Run("commit failure leaves pending", func(t *testing.T) {
		reader, repo := newRecoveryRepo(t)
		cc := &scriptedCommitter{real: repo, fail: true, err: errors.New("commit boom")}
		fi := newFakeInference(summaryTokens("summary"))
		mgr := recoveryManager(t, fi, reader, repo, nil, nil, cc)
		s := startSession(t, mgr)

		if _, err := mgr.Save(context.Background(), s.ID); err == nil {
			t.Fatal("save with a failing commit must fail")
		}
		records, err := ReadAll(reader, sessionsLogRel)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if len(records) != 1 || records[0].State != StatePending {
			t.Fatalf("log after commit failure = %+v, want a single pending record", records)
		}
		if cc.calls != 1 {
			t.Fatalf("commit invocations = %d, want 1", cc.calls)
		}
	})

	t.Run("complete emitted after publication and commit", func(t *testing.T) {
		reader, repo := newRecoveryRepo(t)
		ww := &scriptedWriter{real: reader}
		fi := newFakeInference(summaryTokens("summary"))
		mgr := recoveryManager(t, fi, reader, repo, ww, nil, nil)
		s := startSession(t, mgr)

		if _, err := mgr.Save(context.Background(), s.ID); err != nil {
			t.Fatalf("Save: %v", err)
		}
		// Sidecar first, episode second; the complete record closes the attempt
		// only after both artifacts landed.
		wantWrites := []string{
			"episodes/coder/" + s.ID + ".json",
			"episodes/coder/" + s.ID + ".md",
		}
		if !reflect.DeepEqual(ww.calls, wantWrites) {
			t.Fatalf("artifact write order = %v, want %v", ww.calls, wantWrites)
		}
		records, err := ReadAll(reader, sessionsLogRel)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if len(records) != 2 || records[0].State != StatePending || records[1].State != StateComplete {
			t.Fatalf("log = %+v, want pending then complete", records)
		}
		if records[1].EpisodePath == "" {
			t.Fatal("complete record missing its episode path")
		}
	})
}

// TestSessionRecovery_MonotonicSaveSequence: save attempts are
// monotonically allocated and never reused, counting failed attempts too. Each
// sub-test fails one step of the save lifecycle and proves the consumed attempt
// is skipped by the retry — including failures that occur before any recovery
// record is published, so a reorder that moved allocation after the fallible
// work would keep these failures green.
func TestSessionRecovery_MonotonicSaveSequence(t *testing.T) {
	t.Run("summarizer failure consumes an attempt", func(t *testing.T) {
		fi := newFakeInference(
			summaryTokens("first"),
			[]inference.Token{{Err: errors.New("summarizer down")}},
			summaryTokens("third"),
		)
		reader, repo := newRecoveryRepo(t)
		mgr := recoveryManager(t, fi, reader, repo, nil, nil, nil)
		s := startSession(t, mgr)

		res1, err := mgr.Save(context.Background(), s.ID)
		if err != nil {
			t.Fatalf("save 1: %v", err)
		}
		if _, err := mgr.Save(context.Background(), s.ID); err == nil {
			t.Fatal("save 2 with a failing summarizer must fail")
		}
		res3, err := mgr.Save(context.Background(), s.ID)
		if err != nil {
			t.Fatalf("save 3: %v", err)
		}
		if res1.Attempt != 1 || res3.Attempt != 3 {
			t.Fatalf("result attempts = %d, %d; want 1 then 3", res1.Attempt, res3.Attempt)
		}

		records, err := ReadAll(reader, sessionsLogRel)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		attempts := make([]int, len(records))
		for i, r := range records {
			attempts[i] = r.Attempt
		}
		// pending+complete for attempts 1 and 3, pending only for the failed 2.
		want := []int{1, 1, 2, 3, 3}
		if !reflect.DeepEqual(attempts, want) {
			t.Fatalf("allocated attempts = %v, want %v (strictly increasing; failed attempt 2 consumed and never reused)", attempts, want)
		}
	})

	// The next two sub-tests fail the save before any recovery record is
	// published. If allocation happened after the sidecar/pending work, the
	// retry would restart at attempt 1 and these would pass unchanged; they
	// discriminate that allocation precedes all fallible work.
	t.Run("sidecar write failure consumes an attempt", func(t *testing.T) {
		reader, repo := newRecoveryRepo(t)
		ww := &scriptedWriter{real: reader, fail: 1, err: errors.New("sidecar write boom")}
		fi := newFakeInference(summaryTokens("retry"))
		mgr := recoveryManager(t, fi, reader, repo, ww, nil, nil)
		s := startSession(t, mgr)

		if _, err := mgr.Save(context.Background(), s.ID); err == nil {
			t.Fatal("save with a failing sidecar write must fail")
		}
		res, err := mgr.Save(context.Background(), s.ID)
		if err != nil {
			t.Fatalf("retry after sidecar failure: %v", err)
		}
		if res.Attempt != 2 {
			t.Fatalf("attempt after a consumed sidecar failure = %d, want 2", res.Attempt)
		}
		records, err := ReadAll(reader, sessionsLogRel)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if len(records) != 2 || records[1].Attempt != 2 {
			t.Fatalf("log after retry = %+v, want only the successful attempt 2 pair", records)
		}
	})

	t.Run("pending append failure consumes an attempt", func(t *testing.T) {
		reader, repo := newRecoveryRepo(t)
		aa := &scriptedAppender{real: reader, fail: 1, err: errors.New("pending append boom")}
		fi := newFakeInference(summaryTokens("retry"))
		mgr := recoveryManager(t, fi, reader, repo, nil, aa, nil)
		s := startSession(t, mgr)

		if _, err := mgr.Save(context.Background(), s.ID); err == nil {
			t.Fatal("save with a failing pending append must fail")
		}
		res, err := mgr.Save(context.Background(), s.ID)
		if err != nil {
			t.Fatalf("retry after pending-append failure: %v", err)
		}
		if res.Attempt != 2 {
			t.Fatalf("attempt after a consumed pending-append failure = %d, want 2", res.Attempt)
		}
		records, err := ReadAll(reader, sessionsLogRel)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if len(records) != 2 || records[1].Attempt != 2 {
			t.Fatalf("log after retry = %+v, want only the successful attempt 2 pair", records)
		}
	})
}

// TestSessionRecovery_CompleteSupersedesPending: for one
// attempt, complete deterministically supersedes pending regardless of which
// record appears last in the log or carries the later timestamp.
func TestSessionRecovery_CompleteSupersedesPending(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// Complete first, pending second in physical order, with the pending record
	// carrying the later SavedAt: both order and wall-clock say pending, the
	// state rule says complete.
	records := []Record{
		{ID: "s1", Agent: "coder", Project: "global", SaveSeq: 1, Attempt: 2, State: StateComplete, EpisodePath: "episodes/coder/s1.md", SavedAt: now.Add(-time.Hour)},
		{ID: "s1", Agent: "coder", Project: "global", SaveSeq: 0, Attempt: 2, State: StatePending, EpisodePath: "", SavedAt: now},
	}
	got := LatestPerID(records)
	if len(got) != 1 || got[0].State != StateComplete {
		t.Fatalf("winner = %+v, want the complete record of attempt 2", got)
	}

	// The manager's resume path must agree: it hydrates from the complete
	// record's save_seq, not the pending record that is later in the log.
	reader, repo := newRecoveryRepo(t)
	mgr := recoveryManager(t, newFakeInference(), reader, repo, nil, nil, nil)
	if err := AppendRecord(reader, sessionsLogRel, records[0]); err != nil {
		t.Fatal(err)
	}
	if err := AppendRecord(reader, sessionsLogRel, records[1]); err != nil {
		t.Fatal(err)
	}
	sidecar, err := encodeConversation([]inference.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.WriteFile("episodes/coder/s1.json", sidecar); err != nil {
		t.Fatal(err)
	}
	res, err := mgr.Resume("s1")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.saveSeq != 1 {
		t.Fatalf("resumed saveSeq = %d, want 1 from the complete record", res.saveSeq)
	}
	if res.attempt != 2 {
		t.Fatalf("resumed attempt = %d, want 2", res.attempt)
	}
}

// TestSessionRecovery_NoWallClockOrdering: recovery must not
// use wall-clock ordering. A higher attempt supersedes a lower one even when
// its timestamp is earlier or equal, and regardless of log position.
func TestSessionRecovery_NoWallClockOrdering(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		records []Record
	}{
		{"higher attempt earlier wall clock", []Record{
			{ID: "s1", Agent: "coder", SaveSeq: 0, Attempt: 2, State: StatePending, SavedAt: now.Add(-2 * time.Hour)},
			{ID: "s1", Agent: "coder", SaveSeq: 1, Attempt: 1, State: StateComplete, SavedAt: now},
		}},
		{"equal timestamps", []Record{
			{ID: "s1", Agent: "coder", SaveSeq: 1, Attempt: 1, State: StateComplete, SavedAt: now},
			{ID: "s1", Agent: "coder", SaveSeq: 0, Attempt: 2, State: StatePending, SavedAt: now},
		}},
		{"perturbed physical order", []Record{
			{ID: "s1", Agent: "coder", SaveSeq: 0, Attempt: 2, State: StatePending, SavedAt: now.Add(time.Hour)},
			{ID: "s1", Agent: "coder", SaveSeq: 1, Attempt: 1, State: StateComplete, SavedAt: now.Add(-time.Hour)},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LatestPerID(tc.records)
			if len(got) != 1 {
				t.Fatalf("winner count = %d, want 1", len(got))
			}
			if got[0].Attempt != 2 || got[0].State != StatePending {
				t.Fatalf("winner = %+v, want attempt 2 pending (attempt supersedes wall clock)", got[0])
			}
		})
	}
}

// TestSessionRecovery_BackwardCompatible: existing log records
// without the new attempt/state fields remain readable through the documented
// legacy normalization rule — they are complete, ordered by save_seq — and the
// logs are never rewritten.
func TestSessionRecovery_BackwardCompatible(t *testing.T) {
	reader, repo := newRecoveryRepo(t)
	legacy := strings.Join([]string{
		`{"id":"legacy-1","agent":"coder","project":"global","started_at":"2026-07-01T10:00:00Z","saved_at":"2026-07-01T10:05:00Z","save_seq":1,"episode_path":"episodes/coder/legacy-1.md"}`,
		`{"id":"legacy-1","agent":"coder","project":"global","started_at":"2026-07-01T10:00:00Z","saved_at":"2026-07-01T11:05:00Z","save_seq":2,"episode_path":"episodes/coder/legacy-1.md"}`,
	}, "\n") + "\n"
	if err := reader.WriteFile(sessionsLogRel, []byte(legacy)); err != nil {
		t.Fatalf("write legacy log: %v", err)
	}

	// The new fields are absent on the parsed records (Attempt 0, State empty).
	records, err := ReadAll(reader, sessionsLogRel)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("legacy records = %d, want 2", len(records))
	}
	for _, r := range records {
		if r.Attempt != 0 || r.State != "" {
			t.Fatalf("legacy record parsed with new fields: %+v", r)
		}
	}

	// Legacy normalization: complete, ordered by save_seq; winner is seq 2.
	got := LatestPerID(records)
	if len(got) != 1 || got[0].SaveSeq != 2 {
		t.Fatalf("legacy dedupe = %+v, want the save_seq=2 record", got)
	}
	if got[0].State != "" || effectiveState(got[0]) != StateComplete {
		t.Fatalf("legacy winner must normalize to complete, got %+v", got[0])
	}

	// Resume reads through the legacy rule and continues from save_seq 2.
	sidecar, err := encodeConversation([]inference.Message{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.WriteFile("episodes/coder/legacy-1.json", sidecar); err != nil {
		t.Fatal(err)
	}
	mgr := recoveryManager(t, newFakeInference(summaryTokens("legacy resume")), reader, repo, nil, nil, nil)
	res, err := mgr.Resume("legacy-1")
	if err != nil {
		t.Fatalf("Resume legacy session: %v", err)
	}
	if res.saveSeq != 2 || res.attempt != 2 {
		t.Fatalf("resumed saveSeq/attempt = %d/%d, want 2/2", res.saveSeq, res.attempt)
	}

	// A new-format attempt 3 supersedes the legacy records and stays discoverable.
	if _, err := mgr.Save(context.Background(), res.ID); err != nil {
		t.Fatalf("Save on resumed legacy session: %v", err)
	}
	all, err := ReadAll(reader, sessionsLogRel)
	if err != nil {
		t.Fatal(err)
	}
	winner := LatestPerID(all)[0]
	if winner.Attempt != 3 || winner.State != StateComplete {
		t.Fatalf("winner after legacy resume = %+v, want complete attempt 3", winner)
	}
}

// TestSessionRecovery_FirstSaveDiscoverable: a summarizer
// failure during the very first save must not make the session undiscoverable.
// The pending record keeps it visible and the raw sidecar is resumable.
func TestSessionRecovery_FirstSaveDiscoverable(t *testing.T) {
	fi := newFakeInference([]inference.Token{{Err: errors.New("summarizer down")}})
	reader, repo := newRecoveryRepo(t)
	mgr := recoveryManager(t, fi, reader, repo, nil, nil, nil)
	s := startSession(t, mgr)

	if _, err := mgr.Save(context.Background(), s.ID); err == nil {
		t.Fatal("first save with a failing summarizer must fail")
	}

	records, err := mgr.Records("coder")
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("session undiscoverable after first-save failure: %d records, want 1", len(records))
	}
	if records[0].ID != s.ID || records[0].State != StatePending {
		t.Fatalf("first-save record = %+v, want pending %s", records[0], s.ID)
	}

	got, err := mgr.Resume(s.ID)
	if err != nil {
		t.Fatalf("Resume after first-save failure: %v", err)
	}
	if len(got.Conversation) != 2 {
		t.Fatalf("resumed conversation = %d messages, want 2", len(got.Conversation))
	}
}

// TestSessionRecovery_SidecarFailureEmitsNoPending covers the sidecar failure
// boundary: no pending record is emitted, and the summarizer and episode
// publication are never reached.
func TestSessionRecovery_SidecarFailureEmitsNoPending(t *testing.T) {
	reader, repo := newRecoveryRepo(t)
	ww := &scriptedWriter{real: reader, fail: 1, err: errors.New("sidecar write boom")}
	fi := newFakeInference(summaryTokens("unused"))
	mgr := recoveryManager(t, fi, reader, repo, ww, nil, nil)
	s := startSession(t, mgr)

	_, err := mgr.Save(context.Background(), s.ID)
	if err == nil || !strings.Contains(err.Error(), "write sidecar") {
		t.Fatalf("save error = %v, want a sidecar publication failure", err)
	}
	records, err := ReadAll(reader, sessionsLogRel)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("sidecar failure emitted %d log records, want 0 (no pending)", len(records))
	}
	if fi.calls != 0 {
		t.Fatalf("summarizer invoked %d times after sidecar failure, want 0", fi.calls)
	}
}

// TestSessionRecovery_PendingAppendFailureEmitsNoComplete covers the
// pending-append boundary: the save aborts before summarization and episode
// publication, so no complete record exists.
func TestSessionRecovery_PendingAppendFailureEmitsNoComplete(t *testing.T) {
	reader, repo := newRecoveryRepo(t)
	aa := &scriptedAppender{real: reader, fail: 1, err: errors.New("pending append boom")}
	fi := newFakeInference(summaryTokens("unused"))
	mgr := recoveryManager(t, fi, reader, repo, nil, aa, nil)
	s := startSession(t, mgr)

	_, err := mgr.Save(context.Background(), s.ID)
	if err == nil || !strings.Contains(err.Error(), "append pending") {
		t.Fatalf("save error = %v, want a pending-append failure", err)
	}
	records, err := ReadAll(reader, sessionsLogRel)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("pending append failure left %d records, want 0", len(records))
	}
	if fi.calls != 0 {
		t.Fatalf("summarizer invoked %d times after pending-append failure, want 0", fi.calls)
	}
}

// TestSessionRecovery_EpisodeWriteFailureLeavesOnlyPending covers the episode
// publication boundary: only the pending record remains, never a complete one.
func TestSessionRecovery_EpisodeWriteFailureLeavesOnlyPending(t *testing.T) {
	reader, repo := newRecoveryRepo(t)
	// call 1 = sidecar, call 2 = episode.
	ww := &scriptedWriter{real: reader, fail: 2, err: errors.New("episode write boom")}
	fi := newFakeInference(summaryTokens("summary"))
	mgr := recoveryManager(t, fi, reader, repo, ww, nil, nil)
	s := startSession(t, mgr)

	_, err := mgr.Save(context.Background(), s.ID)
	if err == nil || !strings.Contains(err.Error(), "write episode") {
		t.Fatalf("save error = %v, want an episode publication failure", err)
	}
	records, err := ReadAll(reader, sessionsLogRel)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].State != StatePending {
		t.Fatalf("log after episode failure = %+v, want a single pending record", records)
	}
	if rec, _ := mgr.findLatestRecord(s.ID); rec == nil || rec.State != StatePending {
		t.Fatalf("latest record = %+v, want pending", rec)
	}
}

// TestSessionRecovery_CompleteAppendFailureLeavesPending covers the
// complete-append boundary: the episode is committed, but the failure must not
// manufacture a completed recovery state. The winning state stays pending and
// the session remains resumable from the sidecar.
func TestSessionRecovery_CompleteAppendFailureLeavesPending(t *testing.T) {
	reader, repo := newRecoveryRepo(t)
	aa := &scriptedAppender{real: reader, fail: 2, err: errors.New("complete append boom")}
	fi := newFakeInference(summaryTokens("summary"))
	mgr := recoveryManager(t, fi, reader, repo, nil, aa, nil)
	s := startSession(t, mgr)

	_, err := mgr.Save(context.Background(), s.ID)
	if err == nil || !strings.Contains(err.Error(), "append complete") {
		t.Fatalf("save error = %v, want a complete-append failure", err)
	}
	records, err := ReadAll(reader, sessionsLogRel)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].State != StatePending {
		t.Fatalf("log after complete-append failure = %+v, want a single pending record", records)
	}
	if rec, _ := mgr.findLatestRecord(s.ID); rec == nil || rec.State != StatePending {
		t.Fatalf("latest record = %+v, want pending (no manufactured completion)", rec)
	}
	if _, err := mgr.Resume(s.ID); err != nil {
		t.Fatalf("session must remain resumable after complete-append failure: %v", err)
	}
}

// TestSessionRecovery_ConcurrentSavesDistinctAttempts covers the concurrency
// boundary: concurrent saves stay serialized by saveMu and allocate distinct,
// strictly increasing attempts — no two saves share one attempt number.
func TestSessionRecovery_ConcurrentSavesDistinctAttempts(t *testing.T) {
	const n = 4
	scripts := make([][]inference.Token, n)
	for i := range scripts {
		scripts[i] = summaryTokens(fmt.Sprintf("concurrent %d", i))
	}
	reader, repo := newRecoveryRepo(t)
	mgr := recoveryManager(t, newFakeInference(scripts...), reader, repo, nil, nil, nil)
	s := startSession(t, mgr)

	var wg sync.WaitGroup
	attempts := make(chan int, n)
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := mgr.Save(context.Background(), s.ID)
			if err != nil {
				errs <- err
				return
			}
			attempts <- res.Attempt
		}()
	}
	wg.Wait()
	close(attempts)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent save error: %v", err)
	}
	seen := map[int]bool{}
	for a := range attempts {
		seen[a] = true
	}
	for _, w := range []int{1, 2, 3, 4} {
		if !seen[w] {
			t.Fatalf("concurrent attempts = %v, want exactly 1..4", seen)
		}
	}

	records, err := ReadAll(reader, sessionsLogRel)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 8 {
		t.Fatalf("log records = %d, want 8 (pending + complete per attempt)", len(records))
	}
	for i := range 4 {
		p := records[2*i]
		c := records[2*i+1]
		if p.State != StatePending || c.State != StateComplete || p.Attempt != c.Attempt || p.Attempt != i+1 {
			t.Fatalf("log pair %d = %+v / %+v, want pending+complete on attempt %d", i, p, c, i+1)
		}
	}
}

// TestSessionRecovery_MalformedRecordsExcluded pins the P1 invariant: only
// fully legacy records and valid explicit records participate in recovery
// selection. Malformed hybrids — an unknown state, a state without an attempt,
// an attempt without a state, or negative counters — must be skipped so a
// high-attempt bogus record can never supersede valid history.
func TestSessionRecovery_MalformedRecordsExcluded(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	records := []Record{
		{ID: "s1", Agent: "coder", SaveSeq: 1, Attempt: 1, State: StateComplete, SavedAt: now},
		// Unknown state with a huge attempt must not supersede valid history.
		{ID: "s1", Agent: "coder", SaveSeq: 0, Attempt: 99, State: "pendnig", SavedAt: now.Add(time.Hour)},
		// State without an attempt.
		{ID: "s1", Agent: "coder", SaveSeq: 0, Attempt: 0, State: StatePending, SavedAt: now.Add(2 * time.Hour)},
		// Attempt without a state.
		{ID: "s1", Agent: "coder", SaveSeq: 1, Attempt: 5, State: "", SavedAt: now.Add(3 * time.Hour)},
		// Negative counters.
		{ID: "s1", Agent: "coder", SaveSeq: -1, Attempt: 0, State: "", SavedAt: now.Add(4 * time.Hour)},
		{ID: "s1", Agent: "coder", SaveSeq: 1, Attempt: -2, State: StateComplete, SavedAt: now.Add(5 * time.Hour)},
	}
	got := LatestPerID(records)
	if len(got) != 1 {
		t.Fatalf("winners = %+v, want exactly the valid complete record", got)
	}
	if got[0].State != StateComplete || got[0].Attempt != 1 {
		t.Fatalf("winner = %+v, want the valid complete attempt-1 record", got[0])
	}

	t.Run("ReadAll skips malformed lines", func(t *testing.T) {
		reader, _ := newRecoveryRepo(t)
		logData := strings.Join([]string{
			`{"id":"s1","agent":"coder","save_seq":1,"attempt":1,"state":"complete"}`,
			`{"id":"s2","agent":"coder","save_seq":0,"attempt":99,"state":"pendnig"}`,
		}, "\n") + "\n"
		if err := reader.WriteFile(sessionsLogRel, []byte(logData)); err != nil {
			t.Fatal(err)
		}
		got, err := ReadAll(reader, sessionsLogRel)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != "s1" {
			t.Fatalf("ReadAll = %+v, want only the valid s1 record", got)
		}
	})
}

// TestSessionRecovery_AppendRecordRejectsMalformed pins the P1 invariant that
// the writer enforces the explicit schema: malformed hybrids are refused at
// append time, so the harness can never produce a record that recovery would
// have to skip.
func TestSessionRecovery_AppendRecordRejectsMalformed(t *testing.T) {
	reader, _ := newRecoveryRepo(t)
	cases := []struct {
		name string
		rec  Record
	}{
		// A fully legacy-shaped record (state-less, attempt zero) is correct to
		// READ but must never be WRITTEN: current code always publishes the
		// explicit recovery state.
		{"fully legacy shape", Record{ID: "x"}},
		{"unknown state", Record{ID: "x", Agent: "coder", Attempt: 1, State: "pendnig"}},
		{"state without attempt", Record{ID: "x", Agent: "coder", State: StatePending}},
		{"attempt without state", Record{ID: "x", Agent: "coder", Attempt: 5}},
		{"negative attempt", Record{ID: "x", Agent: "coder", Attempt: -1, State: StateComplete}},
		{"negative save_seq", Record{ID: "x", Agent: "coder", SaveSeq: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := AppendRecord(reader, sessionsLogRel, tc.rec); err == nil {
				t.Fatalf("AppendRecord accepted malformed record %+v", tc.rec)
			}
		})
	}
}
