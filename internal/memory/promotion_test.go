package memory

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

type promotionStoreStub struct {
	files map[string][]byte
}

func (s *promotionStoreStub) Read(relPath string) ([]byte, error) {
	body, ok := s.files[relPath]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), body...), nil
}

func (s *promotionStoreStub) WriteFile(relPath string, data []byte) error {
	if s.files == nil {
		s.files = map[string][]byte{}
	}
	s.files[relPath] = append([]byte(nil), data...)
	return nil
}

func (s *promotionStoreStub) RemoveAll(relPath string) error {
	delete(s.files, relPath)
	return nil
}

type promotionCommitterStub struct {
	err      error
	messages []string
	files    [][]string
	// beforeReturn runs just before Commit returns, so a test can simulate a
	// second writer landing in the window between this call's own write and
	// its rollback.
	beforeReturn func()
}

func (s *promotionCommitterStub) Commit(msg string, files []string) (string, error) {
	s.messages = append(s.messages, msg)
	s.files = append(s.files, append([]string(nil), files...))
	if s.beforeReturn != nil {
		s.beforeReturn()
	}
	if s.err != nil {
		return "", s.err
	}
	return "abc123", nil
}

func TestPromotionServicePromoteFactAppendsAndCommits(t *testing.T) {
	store := &promotionStoreStub{files: map[string][]byte{"facts.md": []byte("existing")}}
	committer := &promotionCommitterStub{}
	svc := PromotionService{Store: store, Committer: committer}

	if err := svc.PromoteFact("new fact"); err != nil {
		t.Fatalf("PromoteFact: %v", err)
	}
	got := string(store.files["facts.md"])
	if !strings.Contains(got, "existing\n\nnew fact\n") {
		t.Fatalf("facts.md = %q", got)
	}
	if len(committer.files) != 1 || committer.files[0][0] != "facts.md" {
		t.Fatalf("commit files = %#v", committer.files)
	}
}

func TestPromotionServiceRollsBackExistingFileWhenCommitFails(t *testing.T) {
	store := &promotionStoreStub{files: map[string][]byte{"facts.md": []byte("existing\n")}}
	committer := &promotionCommitterStub{err: errors.New("git offline")}
	svc := PromotionService{Store: store, Committer: committer}

	err := svc.PromoteFact("new fact")
	if err == nil || !errors.Is(err, committer.err) {
		t.Fatalf("PromoteFact error = %v, want commit error", err)
	}
	if got := string(store.files["facts.md"]); got != "existing\n" {
		t.Fatalf("facts.md after rollback = %q", got)
	}
}

func TestPromotionServiceRemovesNewFileWhenCommitFails(t *testing.T) {
	store := &promotionStoreStub{files: map[string][]byte{}}
	committer := &promotionCommitterStub{err: errors.New("git offline")}
	svc := PromotionService{Store: store, Committer: committer}

	err := svc.AppendAgentNote("coder", "note")
	if err == nil || !errors.Is(err, committer.err) {
		t.Fatalf("AppendAgentNote error = %v, want commit error", err)
	}
	if _, ok := store.files["agents/coder/notes.md"]; ok {
		t.Fatal("new notes file was not removed after commit failure")
	}
}

// A commit failure is rare, but the file this call wrote is not locked while
// it is in flight. A second promotion to the same path landing in that window
// must not be erased by this call's rollback just because this call still
// remembers what it originally wrote.
func TestPromotionServiceRollbackDoesNotClobberAConcurrentWriteToAnExistingFile(t *testing.T) {
	const concurrentWrite = "existing\n\nsomebody else's promotion\n"
	store := &promotionStoreStub{files: map[string][]byte{"facts.md": []byte("existing\n")}}
	committer := &promotionCommitterStub{err: errors.New("git offline")}
	committer.beforeReturn = func() {
		// A different promotion call lands on the same path in the window
		// between this call's write and its failed commit.
		store.files["facts.md"] = []byte(concurrentWrite)
	}
	svc := PromotionService{Store: store, Committer: committer}

	err := svc.PromoteFact("new fact")
	if err == nil || !errors.Is(err, committer.err) {
		t.Fatalf("PromoteFact error = %v, want commit error", err)
	}
	if got := string(store.files["facts.md"]); got != concurrentWrite {
		t.Fatalf("rollback overwrote a concurrent write: facts.md = %q, want %q", got, concurrentWrite)
	}
}

// Same property for the "file did not exist before" path: rollback must not
// remove a file that a concurrent promotion has since written to the same
// name, even though this call's own attempt to create it failed to commit.
func TestPromotionServiceRollbackDoesNotRemoveAConcurrentlyWrittenNewFile(t *testing.T) {
	const concurrentWrite = "somebody else's note\n"
	store := &promotionStoreStub{files: map[string][]byte{}}
	committer := &promotionCommitterStub{err: errors.New("git offline")}
	committer.beforeReturn = func() {
		store.files["agents/coder/notes.md"] = []byte(concurrentWrite)
	}
	svc := PromotionService{Store: store, Committer: committer}

	err := svc.AppendAgentNote("coder", "note")
	if err == nil || !errors.Is(err, committer.err) {
		t.Fatalf("AppendAgentNote error = %v, want commit error", err)
	}
	got, ok := store.files["agents/coder/notes.md"]
	if !ok {
		t.Fatal("rollback removed a file a concurrent promotion had since written")
	}
	if string(got) != concurrentWrite {
		t.Fatalf("notes.md = %q, want the concurrent write %q", got, concurrentWrite)
	}
}
