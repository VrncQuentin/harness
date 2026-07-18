package home

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureCreatesDirectorySkeleton(t *testing.T) {
	root := filepath.Join(t.TempDir(), "harness-home")
	if err := Ensure(root); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, rel := range []string{".", "projects", "logs", "cache"} {
		path := filepath.Join(root, rel)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat %s: %v", rel, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", rel)
		}
	}
}

func TestEnsureRejectsEmptyRoot(t *testing.T) {
	if err := Ensure(""); err == nil {
		t.Fatal("expected empty root error")
	}
}

func TestDBPathUsesHarnessDatabaseFilename(t *testing.T) {
	root := filepath.Join("tmp", "harness")
	want := filepath.Join(root, DBFilename)
	if got := DBPath(root); got != want {
		t.Fatalf("DBPath = %q, want %q", got, want)
	}
}

func TestProjectRepoPathValidatesSlug(t *testing.T) {
	root := filepath.Join("tmp", "harness")
	got, err := ProjectRepoPath(root, "project-1")
	if err != nil {
		t.Fatalf("ProjectRepoPath valid slug: %v", err)
	}
	want := filepath.Join(root, "projects", "project-1")
	if got != want {
		t.Fatalf("ProjectRepoPath = %q, want %q", got, want)
	}
	if _, err := ProjectRepoPath(root, "../escape"); err == nil {
		t.Fatal("expected invalid slug error")
	}
}
