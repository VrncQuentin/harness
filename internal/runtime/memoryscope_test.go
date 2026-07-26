package runtime

import (
	"errors"
	"testing"

	"github.com/VrncQuentin/harness/internal/project"
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
