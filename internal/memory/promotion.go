package memory

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"github.com/VrncQuentin/harness/internal/pathid"
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
// repoWriteLocks holds the actual lock, keyed by the repo's physical identity
// rather than by anything scoped to one request, which is what makes it
// shared across every caller that names the same repository — mirroring the
// pattern internal/git already uses to serialize its own mutations. This
// value's own read-modify-write-commit-and-possible-rollback is one critical
// section under that lock: a second promotion cannot see this one's write,
// its commit, or its rollback partway through.
//
// That lock is not this package's alone to hold, though: rollback's content
// check (below) exists specifically because a promotion is not the only thing
// that writes facts.md or an agent's notes.md — the UI's direct memory-file
// and agent-notes editors do too, through the same Store, entirely outside
// this type. LockRepoWrite is exported so those callers can take the same
// lock around their own write, closing the gap a content check alone cannot:
// a check-then-act sequence only narrows a race against an unlocked writer,
// it never removes it, because there is still a window between the check and
// the act for that writer to land in.
type PromotionService struct {
	Store     PromotionStore
	Committer PromotionCommitter
}

// repoWriteLocks serializes writes to one repository's memory files across
// every caller that takes the lock — PromotionService's own
// read-modify-write-commit-rollback sequence, and any other component
// (notably the UI's direct file editors) that calls LockRepoWrite around its
// own write to the same repo. It has to be keyed by physical identity, not by
// the configured path spelling, for the same reason internal/git's own
// mutation lock is: two handles on one repository reached by different names
// must contend for one lock, not two.
var repoWriteLocks sync.Map // identity key -> *sync.Mutex

// repoIdentifier is satisfied by *DirReader; declared narrowly here so this
// package does not need to import anything to describe the one method it
// needs from a store.
type repoIdentifier interface {
	Identity() pathid.ID
}

// repoWriteLockKey returns the key to serialize writes on, and whether one is
// available. A store that cannot report its own identity — a test fake, most
// likely, or a caller that has nothing but the narrower PromotionStore/
// PromotionCommitter interfaces — gets no lock: production always passes a
// *DirReader.
func repoWriteLockKey(store any) (string, bool) {
	dr, ok := store.(repoIdentifier)
	if !ok {
		return "", false
	}
	return dr.Identity().Key(), true
}

// LockRepoWrite acquires the same per-repository lock PromotionService's own
// read-modify-write-commit-rollback sequence uses, for a caller that writes
// the same memory files by a different route — the UI's direct memory-file
// and agent-notes editors, specifically, which write facts.md and an agent's
// notes.md through the same Store without going through PromotionService at
// all. Call it around that write so a promotion's rollback can never observe
// this call's write as "still present" and then race past it: while this
// lock is held, no promotion's rollback for the same repository can be
// running its own read-compare-and-restore sequence at all.
//
// It returns a no-op unlock if store cannot report its own identity, the same
// graceful degradation the lock's other caller uses; production always passes
// a *memory.DirReader.
func LockRepoWrite(store any) (unlock func()) {
	key, ok := repoWriteLockKey(store)
	if !ok {
		return func() {}
	}
	return lockRepoWriteKey(key)
}

// lockRepoWriteKey acquires the lock for an already-resolved key.
func lockRepoWriteKey(key string) func() {
	actual, _ := repoWriteLocks.LoadOrStore(key, &sync.Mutex{})
	lock, ok := actual.(*sync.Mutex)
	if !ok {
		// Unreachable: only *sync.Mutex is ever stored.
		lock = &sync.Mutex{}
	}
	lock.Lock()
	return lock.Unlock
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
	return s.appendAndCommitHooked(relPath, text, commitMessage, commitContext, nil)
}

// appendAndCommitHooked is appendAndCommit with a hook forwarded to
// rollbackHooked, so a test can stage a write in the narrow window between
// rollback's content check and its restore. Nil on every production path.
func (s PromotionService) appendAndCommitHooked(relPath, text, commitMessage, commitContext string, afterRollbackCheck func()) error {
	if s.Store == nil {
		return errors.New("memory store not available")
	}
	if s.Committer == nil {
		return errors.New("committer not available")
	}
	if key, ok := repoWriteLockKey(s.Store); ok {
		defer lockRepoWriteKey(key)()
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
		if rollbackErr := s.rollbackHooked(relPath, previous, updated, existed, afterRollbackCheck); rollbackErr != nil {
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

// rollbackHooked undoes this call's own write when the commit that was meant
// to follow it fails. expected is the content this call published; rollback
// only proceeds if the file still holds exactly that.
//
// appendAndCommit's repoWriteLocks hold already rules out a second promotion
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
//
// afterCheck runs after the content check passes -- current still equals
// expected, so nothing has visibly raced this call yet -- and before the
// restore or removal that follows. It exists so a test can stage a write
// landing in exactly that window: appendAndCommit still holds repoWriteLocks
// for the whole call at this point, so a LockRepoWrite-respecting writer
// attempting to land here blocks until this call (and its restore) is done,
// which is the property under test. The hook is a parameter, nil on every
// production path, for the same reason every other hook in this codebase is:
// two tests setting shared state at once would each run the other's hook.
func (s PromotionService) rollbackHooked(relPath string, previous, expected []byte, existed bool, afterCheck func()) error {
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
	if afterCheck != nil {
		afterCheck()
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
