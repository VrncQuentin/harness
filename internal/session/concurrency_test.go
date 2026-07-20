package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/project"
)

// TestManager_ConcurrentSaveAndPromotionShareRepoSafely exercises episode
// saves running concurrently with fact promotions. Both paths commit through
// the same *git.Repo, whose mutex is the only thing serializing go-git's
// multi-step worktree add+commit. This guards that contention between the two
// unrelated writers never loses a commit or corrupts the repository.
//
// Promotions run sequentially within one goroutine on purpose: PromotionService
// has no lock of its own, so concurrent promotions would race each other's
// read-modify-write of facts.md — a separate concern from save-vs-promotion.
func TestManager_ConcurrentSaveAndPromotionShareRepoSafely(t *testing.T) {
	const numSaves = 3
	facts := []string{"the sky is blue", "water is wet", "go compiles fast"}

	// Distinct summaries so each save rewrites the episode with new content and
	// produces a real (non-empty) commit; identical bytes would make go-git
	// reject the second save as an empty commit, which is a save-vs-save quirk
	// unrelated to the repo-contention this test targets.
	scripts := make([][]inference.Token, numSaves)
	for i := range scripts {
		scripts[i] = summaryTokens(fmt.Sprintf("concurrent episode summary %d", i))
	}
	fi := newFakeInference(scripts...)

	dir, repo := initRepo(t)
	reader := memory.NewDirReader(dir)
	mgr, err := NewManager(ManagerDeps{
		Repo:               repo,
		Writer:             reader,
		Reader:             reader,
		Inference:          fi,
		SummarizerPrompt:   func() string { return "test prompt" },
		ResolveAbsRepoPath: dir,
	}, project.GlobalSlug)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	promo := memory.PromotionService{Store: reader, Committer: repo}

	s := mgr.Start("coder")
	if err := mgr.Append(s.ID, inference.Message{Role: "user", Content: "remember this"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	errs := make(chan error, numSaves+1)
	var wg sync.WaitGroup

	for i := 0; i < numSaves; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := mgr.Save(context.Background(), s.ID); err != nil {
				errs <- err
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, fact := range facts {
			if err := promo.PromoteFact(fact); err != nil {
				errs <- err
				return
			}
		}
	}()

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent save/promotion error: %v", err)
	}

	// Every promoted fact must be present: sequential promotions must not have
	// lost an append despite contending with saves on the repo mutex.
	factsBody, err := os.ReadFile(filepath.Join(dir, "facts.md"))
	if err != nil {
		t.Fatalf("read facts.md: %v", err)
	}
	for _, fact := range facts {
		if !strings.Contains(string(factsBody), fact) {
			t.Fatalf("facts.md missing %q:\n%s", fact, factsBody)
		}
	}

	// The episode must have landed on disk and in the committed tree.
	episodeRel := episodeMarkdownPath(s.Agent, s.ID)
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(episodeRel))); err != nil {
		t.Fatalf("episode file missing: %v", err)
	}

	// The history must be intact and hold exactly one commit per Commit() call:
	// numSaves episode commits plus one commit per promoted fact. A lost or
	// clobbered commit would change this count.
	wantCommits := numSaves + len(facts)
	if got := countCommits(t, dir); got != wantCommits {
		t.Fatalf("commit count = %d, want %d", got, wantCommits)
	}
}

func countCommits(t *testing.T, dir string) int {
	t.Helper()
	plain, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	head, err := plain.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	iter, err := plain.Log(&gogit.LogOptions{From: head.Hash()})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	defer iter.Close()
	count := 0
	if err := iter.ForEach(func(*object.Commit) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("walk log: %v", err)
	}
	return count
}
