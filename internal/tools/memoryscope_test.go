package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newGitRepoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	initRepoWithCommit(t, dir)
	return dir
}

// noMemoryRepos is the C2 predicate for tests whose repository root is not a
// memory repo. Tests have to supply one: an unset predicate rejects by design.
func noMemoryRepos() MemoryRepoCheck {
	return NewMemoryRepoCheck(func() ([]string, error) { return nil, nil })
}

// memoryScopeOver treats each of paths as a project memory repo.
func memoryScopeOver(paths ...string) MemoryRepoCheck {
	return NewMemoryRepoCheck(func() ([]string, error) { return paths, nil })
}

func TestNewMemoryRepoCheck(t *testing.T) {
	repo := t.TempDir()
	other := t.TempDir()
	boom := errors.New("store offline")

	// The nested path exists on disk deliberately. isMemoryRepo canonicalizes
	// each side with filepath.EvalSymlinks, which fails on a path that is not
	// there and falls back to the raw string — so a missing subdirectory is
	// compared unresolved against a resolved root, and the two disagree
	// whenever the given form is not already canonical (an 8.3 short name such
	// as the RUNNER~1 temp directory on Windows CI). That is a real gap in the
	// predicate rather than a test artifact, and it is out of scope here: this
	// PR changes when the scope is resolved, not how paths are compared. #391
	// closes it by resolving the deepest existing ancestor.
	nested := filepath.Join(repo, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	tests := []struct {
		name    string
		list    func() ([]string, error)
		absRoot string
		want    bool
		wantErr error
	}{
		{
			name:    "root is a memory repo",
			list:    func() ([]string, error) { return []string{repo}, nil },
			absRoot: repo,
			want:    true,
		},
		{
			name:    "root inside a memory repo",
			list:    func() ([]string, error) { return []string{repo}, nil },
			absRoot: nested,
			want:    true,
		},
		{
			name:    "unrelated root",
			list:    func() ([]string, error) { return []string{repo}, nil },
			absRoot: other,
			want:    false,
		},
		{
			name:    "no memory repos configured",
			list:    func() ([]string, error) { return nil, nil },
			absRoot: other,
			want:    false,
		},
		{
			name:    "store error propagates",
			list:    func() ([]string, error) { return nil, boom },
			absRoot: other,
			wantErr: ErrMemoryScopeUnavailable,
		},
		{
			name:    "nil lister",
			list:    nil,
			absRoot: other,
			wantErr: ErrMemoryScopeUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewMemoryRepoCheck(tt.list)(tt.absRoot)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if got {
					t.Error("predicate returned true alongside an error; callers must reject on the error alone")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewMemoryRepoCheckWrapsStoreError(t *testing.T) {
	boom := errors.New("store offline")
	_, err := NewMemoryRepoCheck(func() ([]string, error) { return nil, boom })(t.TempDir())
	// The underlying cause must survive so the UI and logs can show why the
	// write was refused, not just that it was.
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap %v", err, boom)
	}
}

// The C2 lock must consult the project store on every call. A snapshot taken
// when the task started answers for a project layout that may already be gone:
// a repo created or repointed mid-task would go unprotected.
func TestWorkspaceWriteRepoResolvesScopePerCall(t *testing.T) {
	dir := newGitRepoDir(t)

	var memoryPaths []string
	ci := CallInfo{
		SandboxRoots:    []string{dir},
		MemoryRepoCheck: NewMemoryRepoCheck(func() ([]string, error) { return memoryPaths, nil }),
	}
	args := map[string]any{"root": dir}

	if _, _, err := workspaceWriteRepo(ci, args); err != nil {
		t.Fatalf("write allowed before the repo became a memory repo, got: %v", err)
	}

	// The project is now registered as a memory repo, with no new CallInfo.
	memoryPaths = []string{dir}

	_, _, err := workspaceWriteRepo(ci, args)
	if err == nil {
		t.Fatal("write allowed after the root became a memory repo; scope was snapshotted, not resolved per call")
	}
	if !strings.Contains(err.Error(), "C2 scope violation") {
		t.Errorf("err = %v, want a C2 scope violation", err)
	}
}

func TestWorkspaceWriteRepoFailsClosed(t *testing.T) {
	dir := newGitRepoDir(t)

	tests := []struct {
		name  string
		check MemoryRepoCheck
		want  string
	}{
		{
			name:  "predicate not configured",
			check: nil,
			want:  "C2 scope check unavailable",
		},
		{
			name:  "scope cannot be resolved",
			check: NewMemoryRepoCheck(func() ([]string, error) { return nil, errors.New("store offline") }),
			want:  "C2 scope check failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ci := CallInfo{SandboxRoots: []string{dir}, MemoryRepoCheck: tt.check}
			_, _, err := workspaceWriteRepo(ci, map[string]any{"root": dir})
			if err == nil {
				t.Fatal("write allowed when the C2 scope was unknown, want rejection")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
			if !errors.Is(err, ErrMemoryScopeUnavailable) {
				t.Errorf("err = %v, want it to wrap ErrMemoryScopeUnavailable", err)
			}
		})
	}
}

// Every git write tool must be behind the lock, not just the one that happened
// to be tested.
func TestGitWriteToolsFailClosedWithoutScope(t *testing.T) {
	dir := newGitRepoDir(t)
	ci := CallInfo{SandboxRoots: []string{dir}} // no MemoryRepoCheck

	tests := []struct {
		tool Tool
		args map[string]any
	}{
		{tool: &gitCommitTool{}, args: map[string]any{"root": dir, "message": "m"}},
		{tool: &gitBranchTool{}, args: map[string]any{"root": dir, "name": "b"}},
		{tool: &gitCheckoutTool{}, args: map[string]any{"root": dir, "branch": "b"}},
		{tool: &gitPushTool{}, args: map[string]any{"root": dir, "remote": "origin", "branch": "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.tool.ID(), func(t *testing.T) {
			res := tt.tool.Execute(context.Background(), ci, tt.args)
			if res.Error == "" {
				t.Fatalf("%s succeeded without a C2 predicate, want rejection (content: %q)", tt.tool.ID(), res.Content)
			}
			if !strings.Contains(res.Error, "C2 scope check unavailable") {
				t.Errorf("%s error = %q, want a C2 scope-check failure", tt.tool.ID(), res.Error)
			}
		})
	}
}
