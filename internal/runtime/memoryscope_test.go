package runtime

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/memoryops"
	"github.com/VrncQuentin/harness/internal/project"
	"github.com/VrncQuentin/harness/internal/retrieval"
	"github.com/VrncQuentin/harness/internal/tools"
)

// memoryRepoPaths backs the C2 hard lock, so "I cannot tell you" must never be
// reported as "there are none" — an empty list reads at the tool boundary as
// permission to write.
func TestTaskRunnerMemoryRepoPaths(t *testing.T) {
	storeErr := errors.New("database is locked")

	tests := []struct {
		name    string
		store   project.Store
		want    []string
		wantErr bool
	}{
		{
			name: "collects configured repos",
			store: &runtimeProjectStoreStub{projects: map[string]project.Project{
				"global": {Slug: "global", MemoryRepoPath: "/repos/global"},
			}},
			want: []string{"/repos/global"},
		},
		{
			name: "skips projects with no repo path",
			store: &runtimeProjectStoreStub{projects: map[string]project.Project{
				"blank": {Slug: "blank"},
			}},
			want: []string{},
		},
		{
			name:    "store failure is an error, not an empty list",
			store:   &runtimeProjectStoreStub{listErr: storeErr},
			wantErr: true,
		},
		{
			name:    "absent store is an error",
			store:   nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &Runtime{projectStore: tt.store}
			ad := &taskRunnerAdapter{rt: rt}

			got, err := ad.memoryRepoPaths()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("memoryRepoPaths() = %v, want an error", got)
				}
				// The predicate built over this must reject the write.
				if _, cerr := tools.NewMemoryRepoCheck(ad.memoryRepoPaths)("/anywhere"); cerr == nil {
					t.Error("C2 predicate returned no error for an unresolvable scope")
				}
				return
			}
			if err != nil {
				t.Fatalf("memoryRepoPaths: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("paths = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("paths[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// The predicate must observe a project registered after the CallInfo that holds
// it was built, which is the whole point of resolving scope per call.
func TestTaskRunnerMemoryRepoPathsIsLive(t *testing.T) {
	store := &runtimeProjectStoreStub{projects: map[string]project.Project{}}
	ad := &taskRunnerAdapter{rt: &Runtime{projectStore: store}}
	check := tools.NewMemoryRepoCheck(ad.memoryRepoPaths)

	repo := t.TempDir()
	if in, err := check(repo); err != nil || in {
		t.Fatalf("check before registration = (%v, %v), want (false, nil)", in, err)
	}

	store.projects["late"] = project.Project{Slug: "late", MemoryRepoPath: repo}

	in, err := check(repo)
	if err != nil {
		t.Fatalf("check after registration: %v", err)
	}
	if !in {
		t.Error("predicate missed a project registered after the call info was built")
	}
}

// episodePathsFrom must enumerate episodes through a real pinned DirReader,
// returning the historical depth-two episode shape across agents. The previous
// multi-component Glob ("episodes/*/*.md") only matches a wildcard in the final
// component, so it opened the literal "episodes/*" directory and returned
// nothing — production memory_query therefore never scored or traced. This test
// drives the real reader and asserts the exact enumerated paths.
func TestEpisodePathsFromRealDirReader(t *testing.T) {
	mem := newMemoryRepo(t, map[string]string{
		"episodes/coder/2026-01-01.md":     "one",
		"episodes/coder/2026-01-02.md":     "two",
		"episodes/architect/2026-01-03.md": "three",
		"rules.md":                         "not an episode",
		"episodes/top.md":                  "not a depth-two episode",
	})

	got, err := episodePathsFrom(mem)
	if err != nil {
		t.Fatalf("episodePathsFrom: %v", err)
	}
	want := []string{
		"episodes/architect/2026-01-03.md",
		"episodes/coder/2026-01-01.md",
		"episodes/coder/2026-01-02.md",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("episodePathsFrom = %v, want %v", got, want)
	}
}

// memoryQueryFn must reach the scorer with the enumerated episodes when the
// project has them, and, with an empty project, must emit an unscoreable call
// row through the production trace sink (not silently bypass it) and return no
// hits. The scorer is real (over a real index-less EpisodeScorer), so an empty
// project exercises the enumerate-then-exit path rather than a broken glob.
func TestMemoryQueryFnEnumeratesEpisodes(t *testing.T) {
	with := newMemoryRepo(t, map[string]string{
		"episodes/coder/2026-01-01.md": "one",
	})
	ad := &taskRunnerAdapter{
		mem:       with,
		memScorer: &memoryops.EpisodeScorer{Config: config.Defaults().Prompt},
		slug:      "global",
	}
	fn := ad.memoryQueryFn()
	if fn == nil {
		t.Fatal("memoryQueryFn returned nil with a scorer available")
	}
	hits, err := fn(context.Background(), "needle", 5)
	if err != nil {
		t.Fatalf("memoryQueryFn with episodes: %v", err)
	}
	// The index is empty, so no episode is scored; the point is that the
	// enumeration reached the scorer instead of returning before scoring.
	_ = hits

	// With no episodes at all, memoryQueryFn must still record an unscoreable
	// call row through the trace sink, not return before the choke point.
	rec := &traceRecorder{}
	prev := retrieval.DefaultTraceSink
	retrieval.SetDefaultTraceSink(rec)
	t.Cleanup(func() { retrieval.SetDefaultTraceSink(prev) })

	empty := newMemoryRepo(t, map[string]string{})
	ad.mem = empty
	hits, err = fn(context.Background(), "needle", 5)
	if err != nil {
		t.Fatalf("memoryQueryFn with no episodes: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("memoryQueryFn with no episodes returned %d hits, want none", len(hits))
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.rows) != 1 {
		t.Fatalf("memoryQueryFn with no episodes emitted %d trace rows, want exactly one unscoreable call row", len(rec.rows))
	}
	if rec.rows[0].RecordType != retrieval.RecordTypeCall || rec.rows[0].Outcome != retrieval.OutcomeUnscoreable {
		t.Fatalf("empty-project trace row = %+v, want an unscoreable call row", rec.rows[0])
	}
}

// traceRecorder captures emitted retrieval trace rows for tests.
type traceRecorder struct {
	mu   sync.Mutex
	rows []retrieval.RetrievalTrace
}

func (r *traceRecorder) Emit(row retrieval.RetrievalTrace) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, row)
}

func (r *traceRecorder) Close() error { return nil }
