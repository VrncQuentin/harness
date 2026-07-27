package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// swappingDedupChecker simulates a config reload landing in the middle of a
// promotion request: its CheckSimilar, called before the handler writes or
// commits anything, publishes a whole new generation of memory services as a
// side effect. CheckSimilar is where this is realistic to stage — production
// calls out to the embedder over HTTP there, a real, meaningfully long window
// for a reload to land in — rather than at the handler's two-statement read of
// the store and the committer, which offers no comparable window to stage a
// swap into deterministically.
//
// It does not, on its own, prove the handler reads the store and the
// committer from one atomic snapshot rather than two separate ones: by the
// time CheckSimilar runs, both have already been read either way, so this
// passes unchanged whether the handler took one depsSnapshot() call or two.
// It is kept as a real, useful guarantee in its own right — the request the
// dedup check began is the request that gets written and committed, even
// across the slowest step in the handler — and the two-call form is still
// worth avoiding on its own terms: a single snapshot is one atomic read
// instead of a torn one, at zero cost, for a value the rest of the handler
// depends on being self-consistent.
type swappingDedupChecker struct {
	server       *Server
	newStore     *stubMemoryStore
	newCommitter *stubCommitter
}

func (c *swappingDedupChecker) CheckSimilar(_ context.Context, _ string, _ float64) (bool, string, float64, error) {
	c.server.SetServiceDeps(ServiceDeps{
		MemoryStore:             c.newStore,
		Committer:               c.newCommitter,
		Dedup:                   c,
		PromotionDedupThreshold: 0.95,
	})
	return false, "", 0, nil
}

func TestHandlePromoteFact_UsesOneGenerationForTheWholeRequestEvenIfReloadLandsMidRequest(t *testing.T) {
	s := NewServer(3000)
	originalStore := newStubMemoryStore(map[string]string{"facts.md": "existing\n"})
	originalCommitter := &stubCommitter{}
	replacementStore := newStubMemoryStore(map[string]string{"facts.md": "a different repo\n"})
	replacementCommitter := &stubCommitter{}

	checker := &swappingDedupChecker{newStore: replacementStore, newCommitter: replacementCommitter}
	s.SetServiceDeps(ServiceDeps{
		MemoryStore:             originalStore,
		Committer:               originalCommitter,
		Dedup:                   checker,
		PromotionDedupThreshold: 0.95,
	})
	checker.server = s

	form := url.Values{}
	form.Set("text", "new fact")
	req := httptest.NewRequest(http.MethodPost, "/memory/promote", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handlePromoteFact(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if originalStore.lastWritePath != "facts.md" {
		t.Fatalf("the request did not write through the generation it started with: path=%q", originalStore.lastWritePath)
	}
	if replacementStore.lastWritePath != "" {
		t.Fatalf("the write landed on the replacement generation published mid-request: path=%q", replacementStore.lastWritePath)
	}
	if len(originalCommitter.files) != 1 {
		t.Fatalf("the request did not commit through the generation it started with: %#v", originalCommitter.files)
	}
	if len(replacementCommitter.files) != 0 {
		t.Fatalf("the commit landed on the replacement generation published mid-request: %#v", replacementCommitter.files)
	}
}
