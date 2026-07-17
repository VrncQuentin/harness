package memory

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"strings"
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
type PromotionService struct {
	Store     PromotionStore
	Committer PromotionCommitter
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
	previous, existed, err := s.readExisting(relPath)
	if err != nil {
		return err
	}
	updated := appendPromotion(previous, text)
	if err := s.Store.WriteFile(relPath, updated); err != nil {
		return fmt.Errorf("write %s: %w", relPath, err)
	}
	if _, err := s.Committer.Commit(commitMessage, []string{relPath}); err != nil {
		if rollbackErr := s.rollback(relPath, previous, existed); rollbackErr != nil {
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

func (s PromotionService) rollback(relPath string, previous []byte, existed bool) error {
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
