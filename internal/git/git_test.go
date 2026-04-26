package git

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
)

// initRepo creates a fresh non-bare repo in a temp directory and returns
// both the directory path and an opened *Repo handle. Tests that just
// need a working repo on disk use this instead of duplicating the dance.
func initRepo(t *testing.T) (string, *Repo) {
	t.Helper()
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, false); err != nil {
		t.Fatalf("plain init: %v", err)
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return dir, r
}

// writeFile writes data into dir/relPath, creating parent directories as
// needed. relPath uses forward slashes per the Commit contract.
func writeFile(t *testing.T, dir, relPath, data string) {
	t.Helper()
	abs := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", abs, err)
	}
	if err := os.WriteFile(abs, []byte(data), 0o644); err != nil {
		t.Fatalf("write %s: %v", abs, err)
	}
}

func TestOpen(t *testing.T) {
	t.Run("non-existent path returns error", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		if _, err := Open(missing); err == nil {
			t.Fatal("expected error for missing path, got nil")
		}
	})

	t.Run("non-git directory returns error", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := Open(dir); err == nil {
			t.Fatal("expected error for non-git directory, got nil")
		}
	})

	t.Run("freshly initialised repo opens", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := gogit.PlainInit(dir, false); err != nil {
			t.Fatalf("plain init: %v", err)
		}
		r, err := Open(dir)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if r == nil {
			t.Fatal("expected non-nil Repo handle")
		}
	})
}

func TestCommitAndLog(t *testing.T) {
	dir, r := initRepo(t)

	writeFile(t, dir, "projects/global/episodes/coder/2026-04-26.md", "first episode")
	tags := map[string]string{"agent": "coder", "type": "episode"}
	msg := BuildMessage(tags, "first session summary")

	sha, err := r.Commit(msg, []string{"projects/global/episodes/coder/2026-04-26.md"})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(sha) != 40 {
		t.Errorf("expected 40-char hex SHA, got %q", sha)
	}

	got, err := r.Log(nil)
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 commit, got %d", len(got))
	}

	c := got[0]
	if c.SHA != sha {
		t.Errorf("SHA mismatch: log returned %q, commit returned %q", c.SHA, sha)
	}
	if c.Message != "first session summary" {
		t.Errorf("summary mismatch: %q", c.Message)
	}
	if !reflect.DeepEqual(c.Tags, tags) {
		t.Errorf("tags mismatch: got %v, want %v", c.Tags, tags)
	}
	if c.Author == "" {
		t.Error("expected non-empty author")
	}
	if c.Time.IsZero() {
		t.Error("expected non-zero commit time")
	}
}

func TestLog_FilterAndOrdering(t *testing.T) {
	dir, r := initRepo(t)

	// Three commits, two for coder and one for reviewer, committed in that order.
	commits := []struct {
		path  string
		body  string
		tags  map[string]string
		title string
	}{
		{
			path:  "projects/global/episodes/coder/01.md",
			body:  "coder one",
			tags:  map[string]string{"agent": "coder", "type": "episode"},
			title: "coder session one",
		},
		{
			path:  "projects/global/episodes/reviewer/01.md",
			body:  "reviewer one",
			tags:  map[string]string{"agent": "reviewer", "type": "episode"},
			title: "reviewer session one",
		},
		{
			path:  "projects/global/episodes/coder/02.md",
			body:  "coder two",
			tags:  map[string]string{"agent": "coder", "type": "episode"},
			title: "coder session two",
		},
	}

	shas := make([]string, len(commits))
	for i, c := range commits {
		writeFile(t, dir, c.path, c.body)
		sha, err := r.Commit(BuildMessage(c.tags, c.title), []string{c.path})
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		shas[i] = sha
	}

	got, err := r.Log(map[string]string{"agent": "coder"})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 coder commits, got %d", len(got))
	}
	// Newest first: commit index 2 (coder session two), then index 0
	// (coder session one).
	if got[0].Message != "coder session two" {
		t.Errorf("expected newest first, got %q at index 0", got[0].Message)
	}
	if got[1].Message != "coder session one" {
		t.Errorf("expected oldest last, got %q at index 1", got[1].Message)
	}
	if got[0].Tags["agent"] != "coder" || got[1].Tags["agent"] != "coder" {
		t.Errorf("filter leaked non-coder commits: %v / %v", got[0].Tags, got[1].Tags)
	}

	// Empty filter returns everything.
	all, err := r.Log(nil)
	if err != nil {
		t.Fatalf("log nil filter: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 total commits, got %d", len(all))
	}

	// Non-matching filter returns no commits.
	none, err := r.Log(map[string]string{"agent": "ghost"})
	if err != nil {
		t.Fatalf("log empty filter: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("expected 0 commits for missing agent, got %d", len(none))
	}
}

func TestBlobByRef(t *testing.T) {
	dir, r := initRepo(t)
	const want = "first episode body"
	writeFile(t, dir, "projects/global/episodes/coder/01.md", want)

	sha, err := r.Commit("[agent:coder] [type:episode] one", []string{"projects/global/episodes/coder/01.md"})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	got, err := r.BlobByRef(sha)
	if err != nil {
		t.Fatalf("blob by ref: %v", err)
	}
	if string(got) != want {
		t.Errorf("blob mismatch: got %q, want %q", string(got), want)
	}

	t.Run("unknown SHA returns error", func(t *testing.T) {
		_, err := r.BlobByRef("0000000000000000000000000000000000000000")
		if err == nil {
			t.Fatal("expected error for unknown SHA, got nil")
		}
	})
}

func TestBlobByRef_FirstFileSorted(t *testing.T) {
	dir, r := initRepo(t)
	// Two files in one commit. BlobByRef returns bytes of the first file
	// sorted by destination path; "a.md" sorts before "b.md".
	writeFile(t, dir, "a.md", "alpha")
	writeFile(t, dir, "b.md", "beta")

	sha, err := r.Commit("two-file commit", []string{"a.md", "b.md"})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, err := r.BlobByRef(sha)
	if err != nil {
		t.Fatalf("blob by ref: %v", err)
	}
	if string(got) != "alpha" {
		t.Errorf("expected first-by-path bytes, got %q", string(got))
	}
}

func TestBuildMessage_Deterministic(t *testing.T) {
	cases := []struct {
		name    string
		tags    map[string]string
		summary string
		want    string
	}{
		{
			name:    "no tags",
			tags:    map[string]string{},
			summary: "hello world",
			want:    "hello world",
		},
		{
			name:    "single tag",
			tags:    map[string]string{"agent": "coder"},
			summary: "wrote an episode",
			want:    "[agent:coder] wrote an episode",
		},
		{
			name:    "two tags sorted by key",
			tags:    map[string]string{"type": "episode", "agent": "coder"},
			summary: "hello",
			want:    "[agent:coder] [type:episode] hello",
		},
		{
			name:    "empty key dropped",
			tags:    map[string]string{"": "x", "k": "v"},
			summary: "s",
			want:    "[k:v] s",
		},
		{
			name:    "empty value dropped",
			tags:    map[string]string{"k": "", "ok": "ay"},
			summary: "s",
			want:    "[ok:ay] s",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got1 := BuildMessage(tc.tags, tc.summary)
			if got1 != tc.want {
				t.Fatalf("first call: got %q, want %q", got1, tc.want)
			}
			// Run several times: map iteration order is randomised, so
			// repeated calls cover the determinism guarantee.
			for i := 0; i < 8; i++ {
				if g := BuildMessage(tc.tags, tc.summary); g != got1 {
					t.Fatalf("iteration %d: %q != %q", i, g, got1)
				}
			}
		})
	}
}

func TestParseMessage(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantTags    map[string]string
		wantSummary string
	}{
		{
			name:        "plain text without brackets",
			input:       "just a summary",
			wantTags:    map[string]string{},
			wantSummary: "just a summary",
		},
		{
			name:        "single tag",
			input:       "[agent:coder] hello",
			wantTags:    map[string]string{"agent": "coder"},
			wantSummary: "hello",
		},
		{
			name:        "two tags",
			input:       "[agent:coder] [type:episode] wrote an episode",
			wantTags:    map[string]string{"agent": "coder", "type": "episode"},
			wantSummary: "wrote an episode",
		},
		{
			name:        "value containing spaces and colons",
			input:       "[note:hello: world] body",
			wantTags:    map[string]string{"note": "hello: world"},
			wantSummary: "body",
		},
		{
			name:        "unclosed leading bracket falls through",
			input:       "[bad summary",
			wantTags:    map[string]string{},
			wantSummary: "[bad summary",
		},
		{
			name:        "missing colon in leading bracket falls through",
			input:       "[broken] still",
			wantTags:    map[string]string{},
			wantSummary: "[broken] still",
		},
		{
			name:        "later brackets are not parsed",
			input:       "[agent:coder] body with [later:tag]",
			wantTags:    map[string]string{"agent": "coder"},
			wantSummary: "body with [later:tag]",
		},
		{
			name:        "empty summary after tags",
			input:       "[agent:coder] ",
			wantTags:    map[string]string{"agent": "coder"},
			wantSummary: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tags, summary := ParseMessage(tc.input)
			if !reflect.DeepEqual(tags, tc.wantTags) {
				t.Errorf("tags: got %v, want %v", tags, tc.wantTags)
			}
			if summary != tc.wantSummary {
				t.Errorf("summary: got %q, want %q", summary, tc.wantSummary)
			}
		})
	}
}

func TestParseMessage_RoundTripsBuildMessage(t *testing.T) {
	cases := []struct {
		name    string
		tags    map[string]string
		summary string
	}{
		{name: "no tags", tags: map[string]string{}, summary: "hello world"},
		{name: "single tag", tags: map[string]string{"agent": "coder"}, summary: "did a thing"},
		{name: "multiple tags", tags: map[string]string{"agent": "reviewer", "type": "episode"}, summary: "reviewed"},
		{name: "value with spaces", tags: map[string]string{"note": "hello world"}, summary: "body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			built := BuildMessage(tc.tags, tc.summary)
			gotTags, gotSummary := ParseMessage(built)
			if !reflect.DeepEqual(gotTags, tc.tags) {
				t.Errorf("tags after round-trip: got %v, want %v", gotTags, tc.tags)
			}
			if gotSummary != tc.summary {
				t.Errorf("summary after round-trip: got %q, want %q", gotSummary, tc.summary)
			}
		})
	}
}

// TestErrors exercises the error paths that other tests rely on. Each
// returned error wraps the underlying problem with a "git:" prefix per
// the package convention; tests assert the prefix as a regression guard.
func TestErrors(t *testing.T) {
	t.Run("Open wraps with git: prefix", func(t *testing.T) {
		_, err := Open(filepath.Join(t.TempDir(), "missing"))
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.HasPrefix(err.Error(), "git: ") {
			t.Errorf("error not wrapped: %v", err)
		}
	})

	t.Run("BlobByRef on unknown sha returns wrapped error", func(t *testing.T) {
		_, r := initRepo(t)
		_, err := r.BlobByRef("0000000000000000000000000000000000000000")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.HasPrefix(err.Error(), "git: ") {
			t.Errorf("error not wrapped: %v", err)
		}
		// Unwrap should not erase the underlying go-git error.
		if errors.Unwrap(err) == nil {
			t.Error("expected wrapped underlying error")
		}
	})
}
