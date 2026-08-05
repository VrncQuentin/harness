package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/session"
	"github.com/VrncQuentin/harness/internal/ui"
)

// projectSessionsStore reads recent saved sessions from any project's
// memory repo. Unlike the active-project SessionStore, it opens the target
// project's repo on demand so the sidebar and project pages can list
// sessions per project. The count is bounded by the caller (the sidebar
// default lives in the ui package) and never exceeds the log contents.
type projectSessionsStore struct {
	rt *Runtime
}

func (s *projectSessionsStore) Recent(slug string, limit int) ([]ui.SessionRecord, error) {
	if limit <= 0 {
		limit = 5
	}
	rt := s.rt
	rt.mu.Lock()
	store := rt.projectStore
	rt.mu.Unlock()
	if store == nil {
		return nil, nil
	}
	proj, err := store.Get(slug)
	if err != nil {
		return nil, fmt.Errorf("project sessions %s: %w", slug, err)
	}
	reader, err := memory.NewDirReader(proj.MemoryRepoPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("project sessions %s: %w", slug, err)
	}
	defer func() { _ = reader.Close() }()

	records, err := session.ReadAll(reader, session.SessionsLogRel)
	if err != nil {
		return nil, fmt.Errorf("project sessions %s: %w", slug, err)
	}
	latest := session.LatestPerID(records)
	out := make([]ui.SessionRecord, 0, len(latest))
	for _, r := range latest {
		if r.Project != "" && r.Project != slug {
			continue
		}
		out = append(out, ui.SessionRecord{
			ID:      r.ID,
			Agent:   r.Agent,
			SavedAt: r.SavedAt,
			SaveSeq: r.SaveSeq,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavedAt.After(out[j].SavedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
