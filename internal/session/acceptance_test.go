package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/VrncQuentin/harness/internal/agent"
	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/git"
	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/project"
	"github.com/VrncQuentin/harness/internal/prompt"
	gogit "github.com/go-git/go-git/v6"
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
	reader := openTestRepo(t, root)
	mgr, err := NewManager(ManagerDeps{
		Repo:             repo,
		Writer:           reader,
		Reader:           reader,
		Appender:         reader,
		Inference:        fi,
		SummarizerPrompt: func() string { return "test" },
	}, project.GlobalSlug)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr, root
}

// TestEpisodeFileAndCommit covers:
//
//	"Complete a session → episode file appears at
//	 episodes/<agent>/<timestamp>.md, committed to git"
func TestEpisodeFileAndCommit(t *testing.T) {
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

// TestEpisodeCommitMessageFormat covers:
//
//	"Episode commit message matches format [agent:x] [type:episode] ..."
func TestEpisodeCommitMessageFormat(t *testing.T) {
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

// TestSavedEpisodeVisibleToPromptRecency covers:
//
//	"Start a new session → previous episode content appears in the
//	 assembled prompt"
//
// Glue test: drives the real prompt.DiskAssembler against a real
// committed episode and asserts the summary lands in the system message.
func TestSavedEpisodeVisibleToPromptRecency(t *testing.T) {
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
	reader := openTestRepo(t, root)
	active := "coder"
	reg := agent.NewDiskRegistry(reader, func() string { return active }, func(name string) error { active = name; return nil })
	cfg := config.PromptConfig{RecencyN: 5}
	asm := prompt.NewProjectDiskAssembler(reader, reader, reg, cfg)
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

// TestTenSessionsCreateTenEpisodeCommits covers:
//
//	"Complete 10 sessions → all 10 episode files present in git log,
//	 sessions.jsonl has 10 entries"
func TestTenSessionsCreateTenEpisodeCommits(t *testing.T) {
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
	records, err := ReadAll(openTestRepo(t, root), "sessions.jsonl")
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(records) != 10 {
		t.Errorf("expected 10 sessions.jsonl records, got %d", len(records))
	}
}

// TestGarbledSessionLogIsTolerated covers:
//
//	"Corrupt sessions.jsonl by appending garbage →
//	 harness starts without crashing, logs a warning"
func TestGarbledSessionLogIsTolerated(t *testing.T) {
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

	records, err := ReadAll(openTestRepo(t, root), "sessions.jsonl")
	if err != nil {
		t.Fatalf("ReadAll on corrupted log: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("expected 1 parseable record, got %d", len(records))
	}
}

// TestEpisodeVisibleToMemoryWalker is a smoke check that the session writer
// saves episodes at the path the memory browser walker expects. Real handler
// rendering coverage lives in internal/ui tests.
func TestEpisodeVisibleToMemoryWalker(t *testing.T) {
	fi := newFakeInference(summaryTokens("UI test summary"))
	mgr, root := newAcceptanceManager(t, fi)
	s := mgr.Start("coder")
	_ = mgr.Append(s.ID, inference.Message{Role: "user", Content: "ui"})
	if _, err := mgr.Save(context.Background(), s.ID); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reader := openTestRepo(t, root)
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

// TestSidecarMissingResumeError verifies the same condition the UI
// surfaces when the .json sidecar is absent (e.g. fresh clone). The
// session manager returns ErrConversationLost; the UI wraps it in
// ErrSessionConversationLost. We check the manager half here.
func TestSidecarMissingResumeError(t *testing.T) {
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

// openTestRepo pins a project memory repo for a test and closes it on cleanup.
func openTestRepo(t *testing.T, root string) *memory.DirReader {
	t.Helper()
	r, err := memory.OpenDirReader(root)
	if err != nil {
		t.Fatalf("OpenDirReader %s: %v", root, err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}
