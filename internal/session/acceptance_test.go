package session

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/git"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/project"
	"github.com/vrnc/harness/internal/prompt"
)

// scaffoldMemoryRepo creates a temp memory repo with the canonical
// layout the prompt assembler expects (rules + persona + the projects
// tree). Returns the absolute root and an opened *git.Repo.
func scaffoldMemoryRepo(t *testing.T, agentName string) (string, *git.Repo) {
	t.Helper()
	root := t.TempDir()
	if _, err := gogit.PlainInit(root, false); err != nil {
		t.Fatalf("plain init: %v", err)
	}
	files := map[string]string{
		"rules.md": "RULES",
		"user.md":  "USER",
		"facts.md": "FACTS",
		fmt.Sprintf("agents/%s/persona.md", agentName): "PERSONA",
		fmt.Sprintf("agents/%s/rules.md", agentName):   "AGENTRULES",
		fmt.Sprintf("agents/%s/notes.md", agentName):   "NOTES",
	}
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "episodes"), 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "index", "_episodes"), 0o755); err != nil {
		t.Fatalf("mkdir projects/index: %v", err)
	}
	repo, err := git.Open(root)
	if err != nil {
		t.Fatalf("git.Open: %v", err)
	}
	return root, repo
}

func newAcceptanceManager(t *testing.T, fi *fakeInference) (*Manager, string) {
	t.Helper()
	root, repo := scaffoldMemoryRepo(t, "coder")
	reader := memory.NewDirReader(root)
	mgr, err := NewManager(ManagerDeps{
		Repo:               repo,
		Writer:             reader,
		Reader:             reader,
		Inference:          fi,
		SummarizerPrompt:   func() string { return "test" },
		ResolveAbsRepoPath: root,
	}, project.GlobalSlug)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr, root
}

// TestM3Acceptance_1_EpisodeFileAndCommit covers:
//
//	"Complete a session → episode file appears at
//	 episodes/<agent>/<timestamp>.md, committed to git"
func TestM3Acceptance_1_EpisodeFileAndCommit(t *testing.T) {
	fi := newFakeInference(summaryTokens("first sessions summary"))
	mgr, root := newAcceptanceManager(t, fi)
	s := mgr.Start("coder")
	if err := mgr.Append(s.ID, inference.Message{Role: "user", Content: "hi"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	res, err := mgr.Save(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	mdPath := filepath.Join(root, "episodes", "coder", s.ID+".md")
	if _, err := os.Stat(mdPath); err != nil {
		t.Fatalf("expected %s to exist: %v", mdPath, err)
	}

	// Confirm the saved commit is HEAD and carries the structured tags.
	if got := headCommitSHA(t, root); got != res.CommitSHA {
		t.Errorf("HEAD SHA mismatch: head=%s save=%s", got, res.CommitSHA)
	}
	if got := countCommitsWithPrefix(t, root, "[agent:coder] [type:episode] "); got == 0 {
		t.Fatalf("no episode commits in log")
	}
}

// TestM3Acceptance_2_CommitMessageRegex covers:
//
//	"Episode commit message matches format [agent:x] [type:episode] ..."
func TestM3Acceptance_2_CommitMessageRegex(t *testing.T) {
	fi := newFakeInference(summaryTokens("user wants summary regex"))
	mgr, root := newAcceptanceManager(t, fi)
	s := mgr.Start("coder")
	_ = mgr.Append(s.ID, inference.Message{Role: "user", Content: "x"})
	if _, err := mgr.Save(context.Background(), s.ID); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Re-open the repo and read the raw HEAD commit message via go-git.
	gr, err := gogit.PlainOpen(root)
	if err != nil {
		t.Fatalf("plain open: %v", err)
	}
	head, err := gr.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	commit, err := gr.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("commit object: %v", err)
	}
	pattern := regexp.MustCompile(`^\[agent:[^\]]+\] \[type:episode\]`)
	if !pattern.MatchString(commit.Message) {
		t.Errorf("commit message %q does not match %q", commit.Message, pattern.String())
	}
}

// TestM3Acceptance_3_RecencyWiring covers:
//
//	"Start a new session → previous episode content appears in the
//	 assembled prompt"
//
// Glue test: drives the real prompt.DiskAssembler against a real
// committed episode and asserts the summary lands in the system message.
func TestM3Acceptance_3_RecencyWiring(t *testing.T) {
	fi := newFakeInference(
		summaryTokens("FIRST_EPISODE_BODY: covered the failover plan"),
		summaryTokens("SECOND_EPISODE_BODY: discussed the rollback procedure"),
	)
	mgr, root := newAcceptanceManager(t, fi)

	// Save two episodes so the recency layer has something to feed in.
	for i := range 2 {
		s := mgr.Start("coder")
		_ = mgr.Append(s.ID, inference.Message{Role: "user", Content: fmt.Sprintf("turn %d", i)})
		if _, err := mgr.Save(context.Background(), s.ID); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
		// Bump the manager's clock-derived id between saves so the
		// timestamp filenames sort correctly.
		time.Sleep(time.Millisecond)
	}

	// Fresh assembler from the same memory repo. The recency layer
	// reads episodes/<agent>/*.md, which is exactly
	// what the session writer produces.
	reader := memory.NewLayoutV2Reader(root, "global", root)
	active := "coder"
	reg := agent.NewDiskRegistry(reader, func() string { return active }, func(name string) error { active = name; return nil })
	cfg := config.PromptConfig{RecencyN: 5}
	asm := prompt.NewDiskAssembler(reader, reg, cfg)
	msgs, stats, err := asm.Assemble(context.Background(), "coder", []inference.Message{
		{Role: "user", Content: "what is up"},
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatalf("expected at least the system message, got 0")
	}
	sys := msgs[0].Content
	for _, want := range []string{"FIRST_EPISODE_BODY", "SECOND_EPISODE_BODY"} {
		if !strings.Contains(sys, want) {
			t.Errorf("expected %q in system prompt; got:\n%s", want, sys)
		}
	}
	if stats.Episodes == 0 {
		t.Errorf("episodes layer reports 0 tokens")
	}
}

// TestM3Acceptance_4_TenSessions covers:
//
//	"Complete 10 sessions → all 10 episode files present in git log,
//	 sessions.jsonl has 10 entries"
func TestM3Acceptance_4_TenSessions(t *testing.T) {
	scripts := make([][]inference.Token, 0, 10)
	for i := range 10 {
		scripts = append(scripts, summaryTokens(fmt.Sprintf("summary %d", i)))
	}
	fi := newFakeInference(scripts...)
	mgr, root := newAcceptanceManager(t, fi)

	for i := range 10 {
		s := mgr.Start("coder")
		_ = mgr.Append(s.ID, inference.Message{Role: "user", Content: fmt.Sprintf("question %d", i)})
		if _, err := mgr.Save(context.Background(), s.ID); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
		// Tiny sleep to ensure the wall-clock id is unique.
		time.Sleep(2 * time.Millisecond)
	}

	// 10 .md files on disk under the agent dir.
	dir := filepath.Join(root, "episodes", "coder")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	mdCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			mdCount++
		}
	}
	if mdCount != 10 {
		t.Errorf("expected 10 .md files, got %d", mdCount)
	}

	// 10 commits in the log.
	if got := countCommitsWithPrefix(t, root, "[agent:coder] [type:episode] "); got != 10 {
		t.Errorf("expected 10 commits, got %d", got)
	}

	// 10 records in sessions.jsonl.
	logPath := filepath.Join(root, "sessions.jsonl")
	records, err := ReadAll(logPath)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(records) != 10 {
		t.Errorf("expected 10 sessions.jsonl records, got %d", len(records))
	}
}

// TestM3Acceptance_5_GarbledLogTolerated covers:
//
//	"Corrupt sessions.jsonl by appending garbage →
//	 harness starts without crashing, logs a warning"
func TestM3Acceptance_5_GarbledLogTolerated(t *testing.T) {
	fi := newFakeInference(summaryTokens("clean record"))
	mgr, root := newAcceptanceManager(t, fi)
	s := mgr.Start("coder")
	_ = mgr.Append(s.ID, inference.Message{Role: "user", Content: "hi"})
	if _, err := mgr.Save(context.Background(), s.ID); err != nil {
		t.Fatalf("Save: %v", err)
	}

	logPath := filepath.Join(root, "sessions.jsonl")
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString("\nnot-json-garbage\n"); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	_ = f.Close()

	records, err := ReadAll(logPath)
	if err != nil {
		t.Fatalf("ReadAll on corrupted log: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("expected 1 parseable record, got %d", len(records))
	}
}

// TestM3Acceptance_6_UIEpisodeListIntegration is a smoke check that
// the Track D /memory/episodes handler still finds the episodes the
// session writer commits. We don't import the ui package directly to
// keep the dependency graph one-way; instead we exercise the code
// path through the same paths Track D uses (memory.Walker etc) and
// verify the file lands where the handler expects.
func TestM3Acceptance_6_UIEpisodeListIntegration(t *testing.T) {
	fi := newFakeInference(summaryTokens("UI test summary"))
	mgr, root := newAcceptanceManager(t, fi)
	s := mgr.Start("coder")
	_ = mgr.Append(s.ID, inference.Message{Role: "user", Content: "ui"})
	if _, err := mgr.Save(context.Background(), s.ID); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reader := memory.NewDirReader(root)
	entries, err := reader.Walk("episodes")
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	var foundMD bool
	expected := "episodes/coder/" + s.ID + ".md"
	for _, e := range entries {
		if e.Path == expected {
			foundMD = true
			break
		}
	}
	if !foundMD {
		t.Fatalf("episode .md not found via Walk; entries: %+v", entries)
	}
}

// TestM3FullPipelineThroughHTTPHandler is a smoke check that the
// session save endpoint exposed by handleChatSave wires the manager
// adapter end-to-end. We mount a tiny http.ServeMux that drives the
// adapter directly so the test exercises the JSON contract without
// pulling in the full ui.Server template stack.
func TestM3FullPipelineThroughHTTPHandler(t *testing.T) {
	fi := newFakeInference(summaryTokens("http handler summary"))
	mgr, _ := newAcceptanceManager(t, fi)
	s := mgr.Start("coder")
	_ = mgr.Append(s.ID, inference.Message{Role: "user", Content: "hello via http"})

	mux := http.NewServeMux()
	mux.HandleFunc("/save", func(w http.ResponseWriter, r *http.Request) {
		res, err := mgr.Save(r.Context(), s.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := fmt.Fprintf(w, `{"id":%q}`, res.ID); err != nil {
			t.Errorf("write response: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/save", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

// TestM3SidecarMissingResumeError verifies the same condition the UI
// surfaces when the .json sidecar is absent (e.g. fresh clone). The
// session manager returns ErrConversationLost; the UI wraps it in
// ErrSessionConversationLost. We check the manager half here.
func TestM3SidecarMissingResumeError(t *testing.T) {
	fi := newFakeInference(summaryTokens("clean"))
	mgr, root := newAcceptanceManager(t, fi)
	s := mgr.Start("coder")
	_ = mgr.Append(s.ID, inference.Message{Role: "user", Content: "ack"})
	if _, err := mgr.Save(context.Background(), s.ID); err != nil {
		t.Fatalf("Save: %v", err)
	}
	mgr.End(s.ID)
	sidecar := filepath.Join(root, "episodes", "coder", s.ID+".json")
	if err := os.Remove(sidecar); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}
	if _, err := mgr.Resume(s.ID); err == nil {
		t.Fatal("expected error when sidecar is missing")
	}
}
