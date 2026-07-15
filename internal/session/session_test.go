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

	gogit "github.com/go-git/go-git/v5"
	"github.com/vrnc/harness/internal/git"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/project"
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

func (f *fakeInference) Health(_ context.Context) error { return nil }

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
	reader := memory.NewDirReader(dir)
	metricsRec := &fakeMetrics{}
	mgr, err := NewManager(ManagerDeps{
		Repo:               repo,
		Writer:             reader,
		Reader:             reader,
		Inference:          fi,
		Metrics:            metricsRec,
		SummarizerPrompt:   func() string { return "test prompt" },
		ResolveAbsRepoPath: dir,
	}, project.GlobalSlug)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr, reader, dir, metricsRec
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
	matched, err := regexp.MatchString(`^\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}Z(-\d+)?$`, s.ID)
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

	// Sessions log has one entry.
	logPath := filepath.Join(dir, "sessions.jsonl")
	records, err := ReadAll(logPath)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(records))
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
	repo, err := git.Open(dir)
	if err != nil {
		t.Fatalf("git.Open: %v", err)
	}
	commits, err := repo.Log(map[string]string{"agent": "coder", "type": "episode"})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("expected 1 commit, got %d", len(commits))
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

	// Sessions log has two records (append-only).
	logPath := filepath.Join(dir, "sessions.jsonl")
	records, err := ReadAll(logPath)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 log records, got %d", len(records))
	}
	if records[1].SaveSeq != 2 {
		t.Errorf("second log entry seq: want 2, got %d", records[1].SaveSeq)
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
	mgr, reader, dir, _ := newTestManager(t, fi)
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
	if !reader.Exists("episodes/coder/" + s.ID + ".md") {
		t.Fatalf("expected episode .md to survive")
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

	logPath := filepath.Join(dir, "sessions.jsonl")
	records, err := ReadAll(logPath)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 log records, got %d", len(records))
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
