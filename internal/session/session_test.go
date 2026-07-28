package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VrncQuentin/harness/internal/git"
	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/project"
	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// fakeInference returns canned tokens. Each call to Complete consumes
// the next token slice in turn so a single client can drive multiple
// summaries without rebuilding.
type fakeInference struct {
	mu      sync.Mutex
	scripts [][]inference.Token
	cursor  int
	calls   int
	err     error
}

func newFakeInference(scripts ...[]inference.Token) *fakeInference {
	return &fakeInference{scripts: scripts}
}

func (f *fakeInference) Complete(_ context.Context, _ inference.CompletionRequest) (<-chan inference.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var tokens []inference.Token
	if f.cursor < len(f.scripts) {
		tokens = f.scripts[f.cursor]
		f.cursor++
	}
	f.calls++
	ch := make(chan inference.Token, len(tokens)+1)
	for _, t := range tokens {
		ch <- t
	}
	close(ch)
	return ch, nil
}

func summaryTokens(text string) []inference.Token {
	return []inference.Token{
		{Content: text},
		{Done: true},
	}
}

// fakeMetrics records every recorder call so tests can assert metrics
// fired without spinning up a real *sql.DB.
type fakeMetrics struct {
	mu             sync.Mutex
	sessions       []int
	episodes       []int
	commitLatency  []time.Duration
	returnedErrors error
}

func (f *fakeMetrics) SessionCount(n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = append(f.sessions, n)
	return f.returnedErrors
}

func (f *fakeMetrics) EpisodeCount(n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.episodes = append(f.episodes, n)
	return f.returnedErrors
}

func (f *fakeMetrics) GitCommitLatencyMS(d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitLatency = append(f.commitLatency, d)
	return f.returnedErrors
}

// initRepo creates a fresh git repo in a temp dir and returns the
// directory plus opened *git.Repo. Mirrors the helper in internal/git
// tests so the session tests stay self-contained.
func initRepo(t *testing.T) (string, *git.Repo) {
	t.Helper()
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, false); err != nil {
		t.Fatalf("plain init: %v", err)
	}
	r, err := git.Open(dir)
	if err != nil {
		t.Fatalf("git.Open: %v", err)
	}
	return dir, r
}

func newTestManager(t *testing.T, fi *fakeInference) (*Manager, *memory.DirReader, string, *fakeMetrics) {
	t.Helper()
	dir, repo := initRepo(t)
	reader := openTestRepo(t, dir)
	metricsRec := &fakeMetrics{}
	mgr, err := NewManager(ManagerDeps{
		Repo:             repo,
		Writer:           reader,
		Reader:           reader,
		Appender:         reader,
		Inference:        fi,
		Metrics:          metricsRec,
		SummarizerPrompt: func() string { return "test prompt" },
	}, project.GlobalSlug)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr, reader, dir, metricsRec
}

func headCommitSHA(t *testing.T, root string) string {
	t.Helper()
	gr, err := gogit.PlainOpen(root)
	if err != nil {
		t.Fatalf("plain open: %v", err)
	}
	head, err := gr.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	return head.Hash().String()
}

func countCommitsWithPrefix(t *testing.T, root, prefix string) int {
	t.Helper()
	gr, err := gogit.PlainOpen(root)
	if err != nil {
		t.Fatalf("plain open: %v", err)
	}
	head, err := gr.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	iter, err := gr.Log(&gogit.LogOptions{From: head.Hash()})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	defer iter.Close()
	count := 0
	if err := iter.ForEach(func(c *object.Commit) error {
		if strings.HasPrefix(c.Message, prefix) {
			count++
		}
		return nil
	}); err != nil {
		t.Fatalf("iterate log: %v", err)
	}
	return count
}
func TestNewManagerRequiresDependencies(t *testing.T) {
	_, err := NewManager(ManagerDeps{}, project.GlobalSlug)
	if err == nil {
		t.Fatal("expected missing dependency error")
	}
	if !strings.Contains(err.Error(), "ManagerDeps.Repo") {
		t.Fatalf("NewManager error = %v, want missing repo", err)
	}
}
func TestManager_StartMintsValidID(t *testing.T) {
	mgr, _, _, _ := newTestManager(t, newFakeInference(summaryTokens("ok")))
	s := mgr.Start("coder")
	if s.ID == "" {
		t.Fatalf("expected non-empty id")
	}
	matched, err := regexp.MatchString(`^\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}\.\d{9}Z(-\d+)?$`, s.ID)
	if err != nil {
		t.Fatalf("regexp: %v", err)
	}
	if !matched {
		t.Errorf("id %q does not match the expected timestamp shape", s.ID)
	}
	if s.Agent != "coder" {
		t.Errorf("agent: want coder, got %q", s.Agent)
	}
	if s.Project != project.GlobalSlug {
		t.Errorf("project: want %q, got %q", project.GlobalSlug, s.Project)
	}
}

func TestManager_AppendThenSaveWritesFilesAndCommits(t *testing.T) {
	fi := newFakeInference(summaryTokens("user wanted to ship something"))
	mgr, reader, dir, fm := newTestManager(t, fi)
	s := mgr.Start("coder")

	if err := mgr.Append(s.ID, inference.Message{Role: "user", Content: "hi"}); err != nil {
		t.Fatalf("Append user: %v", err)
	}
	if err := mgr.Append(s.ID, inference.Message{Role: "assistant", Content: "hello"}); err != nil {
		t.Fatalf("Append assistant: %v", err)
	}

	res, err := mgr.Save(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatalf("expected non-empty commit sha")
	}
	if res.SaveSeq != 1 {
		t.Errorf("save_seq: want 1, got %d", res.SaveSeq)
	}

	// Episode .md is committed; sidecar .json is on disk only.
	mdPath := filepath.Join(dir, "episodes", "coder", s.ID+".md")
	jsonPath := filepath.Join(dir, "episodes", "coder", s.ID+".json")
	if _, err := os.Stat(mdPath); err != nil {
		t.Fatalf("expected .md to exist: %v", err)
	}
	if _, err := os.Stat(jsonPath); err != nil {
		t.Fatalf("expected .json sidecar to exist: %v", err)
	}

	// Sessions log has two entries for this session's first save: a
	// provisional record (written before the summarizer runs, so the
	// session is discoverable even if it fails) and the full record that
	// supersedes it once the save actually succeeds.
	records, err := ReadAll(openTestRepo(t, dir), "sessions.jsonl")
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 log records (provisional + final), got %d", len(records))
	}

	// Sidecar round-trips back to the same conversation.
	body, err := reader.Read("episodes/coder/" + s.ID + ".json")
	if err != nil {
		t.Fatalf("Read sidecar: %v", err)
	}
	conv, err := decodeConversation(body)
	if err != nil {
		t.Fatalf("decodeConversation: %v", err)
	}
	if len(conv) != 2 {
		t.Fatalf("sidecar conversation: want 2 messages, got %d", len(conv))
	}

	// Commit message starts with the structured tag prefix.
	if got := countCommitsWithPrefix(t, dir, "[agent:coder] [type:episode] "); got != 1 {
		t.Fatalf("expected 1 episode commit, got %d", got)
	}

	// Metrics fired once.
	if len(fm.sessions) != 1 || fm.sessions[0] != 1 {
		t.Errorf("session_count: want [1], got %v", fm.sessions)
	}
	if len(fm.episodes) != 1 || fm.episodes[0] != 1 {
		t.Errorf("episode_count: want [1], got %v", fm.episodes)
	}
	if len(fm.commitLatency) != 1 {
		t.Errorf("git_commit_latency_ms: want 1 sample, got %d", len(fm.commitLatency))
	}
}

// Save used to call the summarizer before writing the sidecar. A summarizer
// failure (plausible exactly when a reload's last-minute FlushAll runs,
// since llama-server can be mid-reconfiguration at that moment) meant Save
// returned before either file was written -- the raw conversation was lost
// entirely, not just the markdown summary. This proves the fix: even though
// Save still returns an error when the summarizer fails, the sidecar (raw
// conversation JSON) has already been durably written by the time it does.
func TestManager_SavePersistsSidecarEvenWhenSummarizerFails(t *testing.T) {
	fi := newFakeInference()
	fi.err = errors.New("llama-server unavailable")
	mgr, reader, _, _ := newTestManager(t, fi)
	s := mgr.Start("coder")

	if err := mgr.Append(s.ID, inference.Message{Role: "user", Content: "hi"}); err != nil {
		t.Fatalf("Append user: %v", err)
	}
	if err := mgr.Append(s.ID, inference.Message{Role: "assistant", Content: "hello"}); err != nil {
		t.Fatalf("Append assistant: %v", err)
	}

	_, err := mgr.Save(context.Background(), s.ID)
	if err == nil {
		t.Fatal("Save: want an error from the failing summarizer, got nil")
	}

	body, readErr := reader.Read("episodes/coder/" + s.ID + ".json")
	if readErr != nil {
		t.Fatalf("sidecar was not durably written despite the summarizer failure: %v", readErr)
	}
	conv, err := decodeConversation(body)
	if err != nil {
		t.Fatalf("decodeConversation: %v", err)
	}
	if len(conv) != 2 {
		t.Fatalf("sidecar conversation: want 2 messages, got %d", len(conv))
	}
}

// TestManager_ResumeFindsSessionAfterFirstSaveSummarizerFailure proves the
// stronger guarantee a durable sidecar alone does not: a brand-new session
// whose first Save fails partway through (summarizer error, after the
// sidecar write) must still be discoverable by a later Resume, not just
// readable by someone salvaging bytes off disk by hand.
func TestManager_ResumeFindsSessionAfterFirstSaveSummarizerFailure(t *testing.T) {
	fi := newFakeInference()
	fi.err = errors.New("llama-server unavailable")
	mgr, reader, _, _ := newTestManager(t, fi)
	s := mgr.Start("coder")

	if err := mgr.Append(s.ID, inference.Message{Role: "user", Content: "hi"}); err != nil {
		t.Fatalf("Append user: %v", err)
	}
	if err := mgr.Append(s.ID, inference.Message{Role: "assistant", Content: "hello"}); err != nil {
		t.Fatalf("Append assistant: %v", err)
	}

	if _, err := mgr.Save(context.Background(), s.ID); err == nil {
		t.Fatal("Save: want an error from the failing summarizer, got nil")
	}

	// Simulate a fresh manager (e.g. after a reload) that has never seen
	// this session live in memory - it can only discover it through
	// sessions.jsonl, exactly like the reviewer's "new resume picker".
	fresh, err := NewManager(ManagerDeps{
		Repo:             mgr.deps.Repo,
		Writer:           reader,
		Reader:           reader,
		Appender:         reader,
		Inference:        fi,
		Metrics:          &fakeMetrics{},
		SummarizerPrompt: func() string { return "test prompt" },
	}, project.GlobalSlug)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	resumed, err := fresh.Resume(s.ID)
	if err != nil {
		t.Fatalf("Resume after failed first save: want success, got %v", err)
	}
	if len(resumed.Conversation) != 2 {
		t.Fatalf("resumed conversation: want 2 messages, got %d", len(resumed.Conversation))
	}
	if resumed.Conversation[0].Content != "hi" || resumed.Conversation[1].Content != "hello" {
		t.Fatalf("resumed conversation content mismatch: %+v", resumed.Conversation)
	}
}

func TestManager_SaveTwiceIncrementsSeqAndOverwrites(t *testing.T) {
	fi := newFakeInference(summaryTokens("first summary"), summaryTokens("second summary"))
	mgr, reader, dir, fm := newTestManager(t, fi)
	s := mgr.Start("reviewer")
	if err := mgr.Append(s.ID, inference.Message{Role: "user", Content: "what is up"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := mgr.Save(context.Background(), s.ID); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	if err := mgr.Append(s.ID, inference.Message{Role: "assistant", Content: "all good"}); err != nil {
		t.Fatalf("Append 2: %v", err)
	}
	res, err := mgr.Save(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	if res.SaveSeq != 2 {
		t.Errorf("save_seq: want 2, got %d", res.SaveSeq)
	}

	// Sessions log has three records (append-only): the first save's
	// provisional record plus its final one, then the second save's final
	// record. Only a session's first save writes a provisional entry (see
	// Save's own comment) -- the second save here is already known, so it
	// contributes just the one record.
	records, err := ReadAll(openTestRepo(t, dir), "sessions.jsonl")
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("expected 3 log records (save 1's provisional + final, save 2's final), got %d", len(records))
	}
	if records[2].SaveSeq != 2 {
		t.Errorf("third log entry seq: want 2, got %d", records[2].SaveSeq)
	}

	// Episode markdown was overwritten with the latest summary.
	body, err := reader.Read("episodes/reviewer/" + s.ID + ".md")
	if err != nil {
		t.Fatalf("Read episode: %v", err)
	}
	if !strings.Contains(string(body), "second summary") {
		t.Errorf("episode body should reflect latest summary, got: %q", string(body))
	}

	// SessionCount only fires on first save of an id (gauge stays at 1).
	if len(fm.sessions) != 1 {
		t.Errorf("session_count: want 1 sample (first save only), got %d", len(fm.sessions))
	}
	if len(fm.episodes) != 2 {
		t.Errorf("episode_count: want 2 samples, got %d", len(fm.episodes))
	}
}

func TestManager_ConcurrentSavesSerializeSaveSeq(t *testing.T) {
	fi := newFakeInference(summaryTokens("first concurrent summary"), summaryTokens("second concurrent summary"))
	mgr, _, dir, _ := newTestManager(t, fi)
	s := mgr.Start("coder")
	if err := mgr.Append(s.ID, inference.Message{Role: "user", Content: "save this safely"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	type saveOutcome struct {
		seq int
		err error
	}
	out := make(chan saveOutcome, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := mgr.Save(context.Background(), s.ID)
			out <- saveOutcome{seq: res.SaveSeq, err: err}
		}()
	}
	wg.Wait()
	close(out)

	seenSeq := map[int]bool{}
	for got := range out {
		if got.err != nil {
			t.Fatalf("Save returned error: %v", got.err)
		}
		seenSeq[got.seq] = true
	}
	if !seenSeq[1] || !seenSeq[2] || len(seenSeq) != 2 {
		t.Fatalf("concurrent save seqs = %#v, want exactly 1 and 2", seenSeq)
	}

	// saveMu fully serializes the two "concurrent" Save calls, so whichever
	// one actually runs first sees a brand-new session (no earlier record)
	// and writes both a provisional and a final entry at seq 1; whichever
	// runs second is already known by then and writes only its final entry
	// at seq 2 -- three records in total, in that order, regardless of which
	// goroutine happened to win the race for saveMu.
	records, err := ReadAll(openTestRepo(t, dir), "sessions.jsonl")
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("log records = %d, want 3", len(records))
	}
	if records[0].SaveSeq != 1 || records[1].SaveSeq != 1 || records[2].SaveSeq != 2 {
		t.Fatalf("log save seqs = %d,%d,%d; want 1,1,2", records[0].SaveSeq, records[1].SaveSeq, records[2].SaveSeq)
	}
}
func TestManager_ResumeHydratesConversation(t *testing.T) {
	fi := newFakeInference(summaryTokens("yo"))
	mgr, _, _, _ := newTestManager(t, fi)
	s := mgr.Start("coder")
	want := inference.Message{Role: "user", Content: "remembered turn"}
	if err := mgr.Append(s.ID, want); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := mgr.Append(s.ID, inference.Message{Role: "assistant", Content: "ack"}); err != nil {
		t.Fatalf("Append 2: %v", err)
	}
	if _, err := mgr.Save(context.Background(), s.ID); err != nil {
		t.Fatalf("Save: %v", err)
	}
	mgr.End(s.ID)

	got, err := mgr.Resume(s.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(got.Conversation) != 2 {
		t.Fatalf("Resume conversation: want 2, got %d", len(got.Conversation))
	}
	if !reflect.DeepEqual(got.Conversation[0], want) {
		t.Errorf("first message: want %+v, got %+v", want, got.Conversation[0])
	}
}

func TestManager_ResumeMissingSidecarErrConversationLost(t *testing.T) {
	fi := newFakeInference(summaryTokens("yo"))
	mgr, _, dir, _ := newTestManager(t, fi)
	s := mgr.Start("coder")
	if err := mgr.Append(s.ID, inference.Message{Role: "user", Content: "saved"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := mgr.Save(context.Background(), s.ID); err != nil {
		t.Fatalf("Save: %v", err)
	}
	mgr.End(s.ID)

	// Simulate a fresh clone: delete the sidecar but keep the .md and the log.
	sidecar := filepath.Join(dir, "episodes", "coder", s.ID+".json")
	if err := os.Remove(sidecar); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}
	// Sanity: the .md is still there.
	if _, err := os.Stat(filepath.Join(dir, "episodes", "coder", s.ID+".md")); err != nil {
		t.Fatalf("expected episode .md to survive: %v", err)
	}

	if _, err := mgr.Resume(s.ID); !errors.Is(err, ErrConversationLost) {
		t.Fatalf("Resume: want ErrConversationLost, got %v", err)
	}
}

func TestManager_ResumeUnknownID(t *testing.T) {
	fi := newFakeInference()
	mgr, _, _, _ := newTestManager(t, fi)
	if _, err := mgr.Resume("never-saved"); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("Resume unknown: want ErrUnknownSession, got %v", err)
	}
}

func TestManager_AppendUnknown(t *testing.T) {
	fi := newFakeInference()
	mgr, _, _, _ := newTestManager(t, fi)
	if err := mgr.Append("never-started", inference.Message{Role: "user", Content: "x"}); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("Append unknown: want ErrUnknownSession, got %v", err)
	}
}

func TestManager_FlushAllSavesEveryLiveSession(t *testing.T) {
	fi := newFakeInference(summaryTokens("first"), summaryTokens("second"))
	mgr, _, dir, _ := newTestManager(t, fi)

	a := mgr.Start("coder")
	b := mgr.Start("reviewer")
	if err := mgr.Append(a.ID, inference.Message{Role: "user", Content: "a"}); err != nil {
		t.Fatalf("Append a: %v", err)
	}
	if err := mgr.Append(b.ID, inference.Message{Role: "user", Content: "b"}); err != nil {
		t.Fatalf("Append b: %v", err)
	}
	if err := mgr.FlushAll(context.Background()); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}

	// Both sessions are brand new, so each of the two saves FlushAll performs
	// is a first save: a provisional record plus its final one, per session.
	records, err := ReadAll(openTestRepo(t, dir), "sessions.jsonl")
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("expected 4 log records (2 sessions x provisional+final), got %d", len(records))
	}
}

func TestManager_RecordsFiltersByAgentAndDedupes(t *testing.T) {
	fi := newFakeInference(summaryTokens("s"), summaryTokens("s2"), summaryTokens("s3"))
	mgr, _, _, _ := newTestManager(t, fi)
	a := mgr.Start("coder")
	b := mgr.Start("reviewer")
	for _, id := range []string{a.ID, b.ID} {
		if err := mgr.Append(id, inference.Message{Role: "user", Content: "hi"}); err != nil {
			t.Fatalf("Append %s: %v", id, err)
		}
	}
	if _, err := mgr.Save(context.Background(), a.ID); err != nil {
		t.Fatalf("Save a: %v", err)
	}
	if _, err := mgr.Save(context.Background(), a.ID); err != nil {
		t.Fatalf("Save a 2: %v", err)
	}
	if _, err := mgr.Save(context.Background(), b.ID); err != nil {
		t.Fatalf("Save b: %v", err)
	}

	got, err := mgr.Records("coder")
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 deduped record for coder, got %d", len(got))
	}
	if got[0].SaveSeq != 2 {
		t.Errorf("coder dedupe: want save_seq=2, got %d", got[0].SaveSeq)
	}
}

func TestSummarizer_DefaultsAndError(t *testing.T) {
	t.Run("defaults to fallback when prompt empty", func(t *testing.T) {
		fi := newFakeInference(summaryTokens("ok"))
		s := NewSummarizer(fi, func() string { return "" }, time.Second)
		got, err := s.Summarize(context.Background(), []inference.Message{{Role: "user", Content: "hi"}})
		if err != nil {
			t.Fatalf("Summarize: %v", err)
		}
		if got != "ok" {
			t.Errorf("want %q, got %q", "ok", got)
		}
	})

	t.Run("error when inference returns error token", func(t *testing.T) {
		fi := newFakeInference([]inference.Token{{Err: errors.New("boom")}})
		s := NewSummarizer(fi, func() string { return "" }, time.Second)
		_, err := s.Summarize(context.Background(), []inference.Message{{Role: "user", Content: "hi"}})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("error when conversation empty", func(t *testing.T) {
		fi := newFakeInference()
		s := NewSummarizer(fi, func() string { return "" }, time.Second)
		_, err := s.Summarize(context.Background(), nil)
		if err == nil {
			t.Fatal("expected error for empty conversation")
		}
	})

	t.Run("error when client errors", func(t *testing.T) {
		fi := &fakeInference{err: errors.New("client down")}
		s := NewSummarizer(fi, func() string { return "" }, time.Second)
		_, err := s.Summarize(context.Background(), []inference.Message{{Role: "user", Content: "hi"}})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestEncodeDecodeConversationRoundTrip(t *testing.T) {
	want := []inference.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "world"},
	}
	body, err := encodeConversation(want)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeConversation(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len: want %d, got %d", len(want), len(got))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("msg %d: want %+v, got %+v", i, want[i], got[i])
		}
	}
}

func TestDecodeConversation_StripsBOM(t *testing.T) {
	bom := []byte{0xEF, 0xBB, 0xBF}
	body := []byte(`[{"role":"user","content":"hi"}]`)
	payload := make([]byte, 0, len(bom)+len(body))
	payload = append(payload, bom...)
	payload = append(payload, body...)
	got, err := decodeConversation(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Content != "hi" {
		t.Errorf("BOM-prefixed body did not decode: %+v", got)
	}
}

func TestManager_StartReservesIDsAfterEnd(t *testing.T) {
	mgr, _, _, _ := newTestManager(t, newFakeInference(summaryTokens("ok")))
	fixed := time.Date(2026, time.July, 17, 7, 0, 0, 0, time.UTC)
	mgr.deps.Now = func() time.Time { return fixed }

	first := mgr.Start("coder")
	mgr.End(first.ID)
	second := mgr.Start("coder")
	mgr.End(second.ID)
	third := mgr.Start("coder")

	seen := map[string]bool{}
	for _, id := range []string{first.ID, second.ID, third.ID} {
		if seen[id] {
			t.Fatalf("sequential sessions reused id %q", id)
		}
		seen[id] = true
	}
}
