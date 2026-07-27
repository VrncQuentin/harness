package memory

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
)

// PromotionStore is the mutable memory repo surface required by promotion
// workflows.
type PromotionStore interface {
	Read(relPath string) ([]byte, error)
	WriteFile(relPath string, data []byte) error
}

// PromotionCommitter commits promoted memory files after they are written.
type PromotionCommitter interface {
	Commit(msg string, files []string) (string, error)
}

type promotionRemover interface {
	RemoveAll(relPath string) error
}

// PromotionService owns append-and-commit memory promotions so UI handlers do
// not need to coordinate file mutation, git commits, and rollback.
//
// A value is constructed fresh per request, so a mutex on the value itself
// would not serialize anything: two concurrent promotions to the same repo
// each get their own PromotionService and their own uncontended mutex.
// promotionLocks holds the actual lock, keyed by the repo's physical identity
// rather than by anything scoped to one request, which is what makes it shared
// across every PromotionService instance that names the same repository —
// mirroring the pattern internal/git already uses to serialize its own
// mutations. Read-modify-write-commit-and-possible-rollback is one critical
// section: a second promotion cannot see this one's write, its commit, or its
// rollback partway through, which removes the read-then-clobber gap a
// content check alone cannot close on its own.
type PromotionService struct {
	Store     PromotionStore
	Committer PromotionCommitter
}

// promotionLocks serializes promotion read-modify-write-commit sequences per
// repository, the same role internal/git.repoMutationLocks plays for git
// mutations. It has to be keyed by physical identity, not by the configured
// path spelling, for the same reason that package's lock is: two handles on
// one repository reached by different names must contend for one lock, not
// two.
var promotionLocks sync.Map // identity key -> *sync.Mutex

// promotionLockKey returns the key to serialize promotions on, and whether one
// is available. A store that cannot report its own identity — a test fake,
// most likely — gets no lock: production always passes a *DirReader.
func promotionLockKey(store PromotionStore) (string, bool) {
	dr, ok := store.(*DirReader)
	if !ok {
		return "", false
	}
	return dr.Identity().Key(), true
}

// PromoteFact appends text to facts.md and commits the change. If the commit
// fails, the previous file content is restored before returning the error.
func (s PromotionService) PromoteFact(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("text is required")
	}
	return s.appendAndCommit("facts.md", text, "[type:fact] promote fact", "commit fact")
}

// AppendAgentNote appends text to an agent notes file and commits the change.
// If the commit fails, the previous file content is restored before returning
// the error.
func (s PromotionService) AppendAgentNote(agentName, text string) error {
	agentName = strings.TrimSpace(agentName)
	if !validPromotionAgentName(agentName) {
		return errors.New("valid agent name is required")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("text is required")
	}
	notePath := fmt.Sprintf("agents/%s/notes.md", agentName)
	return s.appendAndCommit(notePath, text, fmt.Sprintf("[agent:%s] [type:note] agent note", agentName), "commit note")
}

func (s PromotionService) appendAndCommit(relPath, text, commitMessage, commitContext string) error {
	if s.Store == nil {
		return errors.New("memory store not available")
	}
	if s.Committer == nil {
		return errors.New("committer not available")
	}
	if key, ok := promotionLockKey(s.Store); ok {
		actual, _ := promotionLocks.LoadOrStore(key, &sync.Mutex{})
		lock, ok := actual.(*sync.Mutex)
		if !ok {
			// Unreachable: only *sync.Mutex is ever stored.
			lock = &sync.Mutex{}
		}
		lock.Lock()
		defer lock.Unlock()
	}
	previous, existed, err := s.readExisting(relPath)
	if err != nil {
		return err
	}
	updated := appendPromotion(previous, text)
	if err := s.Store.WriteFile(relPath, updated); err != nil {
		return fmt.Errorf("write %s: %w", relPath, err)
	}
	if _, err := s.Committer.Commit(commitMessage, []string{relPath}); err != nil {
		if rollbackErr := s.rollback(relPath, previous, updated, existed); rollbackErr != nil {
			return fmt.Errorf("%s: %w; rollback: %v", commitContext, err, rollbackErr)
		}
		return fmt.Errorf("%s: %w", commitContext, err)
	}
	return nil
}

func (s PromotionService) readExisting(relPath string) ([]byte, bool, error) {
	existing, err := s.Store.Read(relPath)
	if err == nil {
		return existing, true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("read %s: %w", relPath, err)
}

// rollback undoes this call's own write when the commit that was meant to
// follow it fails. expected is the content this call published; rollback only
// proceeds if the file still holds exactly that.
//
// appendAndCommit's promotionLocks hold already rules out a second promotion
// landing here — this call's own read, write, and any rollback are one
// critical section for that repository, so another PromotionService call
// cannot interleave partway through. What the content check guards against is
// a *different* writer to the same file that does not participate in that
// lock — the UI's direct memory-file editor, most plausibly, since it writes
// through the same Store without going through PromotionService at all.
// Restoring or removing by name alone, on the strength of what this call
// remembers having written, would destroy such a writer's content — the same
// unlink-what-the-name-now-holds hazard documented on CreateExclusive and
// WriteStreamAtomic elsewhere in this codebase, applied here to a rollback
// instead of a create. Reading the file back and comparing it to what this
// call wrote is the check that closes it: if the content no longer matches,
// somebody else has already written over this call's promotion, and rolling
// back would erase their write instead of this one's, so the safe choice is to
// leave the file exactly as it is and let the original commit error stand.
func (s PromotionService) rollback(relPath string, previous, expected []byte, existed bool) error {
	current, err := s.Store.Read(relPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Already gone by some other route; there is nothing of this
			// call's to undo and nothing here to destroy.
			return nil
		}
		return fmt.Errorf("read %s before rollback: %w", relPath, err)
	}
	if !bytes.Equal(current, expected) {
		return nil
	}
	if existed {
		return s.Store.WriteFile(relPath, previous)
	}
	if remover, ok := s.Store.(promotionRemover); ok {
		return remover.RemoveAll(relPath)
	}
	return s.Store.WriteFile(relPath, nil)
}

func appendPromotion(existing []byte, text string) []byte {
	var builder strings.Builder
	builder.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		builder.WriteByte('\n')
	}
	builder.WriteString("\n")
	builder.WriteString(text)
	builder.WriteString("\n")
	return []byte(builder.String())
}

func validPromotionAgentName(name string) bool {
	if name == "" || name == "." || name == ".." || name[0] == '.' || name[0] == '-' {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return !strings.ContainsAny(name, "/\\")
}
