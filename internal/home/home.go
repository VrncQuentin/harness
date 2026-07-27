// Package home resolves the machine-local harness home directory.
package home

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/VrncQuentin/harness/internal/project"
)

const (
	DirName    = ".harness"
	DBFilename = "harness.db"
)

// Default returns the default harness home, currently ~/.harness.
func Default() (string, error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home: resolve user home: %w", err)
	}
	if userHome == "" {
		return "", fmt.Errorf("home: user home is empty")
	}
	return filepath.Join(userHome, DirName), nil
}

// Ensure creates the stable machine-local directory skeleton.
//
// This is the bootstrap that brings the harness home into existence, so there
// is no enclosing handle to resolve it through: every rooted capability in the
// harness is opened on a directory this call created. Nothing is read or
// written here — only directories are created, and MkdirAll accepts an existing
// directory and refuses anything else, so a name already taken by a file or a
// link to elsewhere fails rather than being adopted. See the filesystem access
// ledger in docs/architecture.md.
func Ensure(root string) error {
	if root == "" {
		return fmt.Errorf("home: root is empty")
	}
	for _, dir := range []string{
		root,
		filepath.Join(root, "projects"),
		filepath.Join(root, "logs"),
		filepath.Join(root, "cache"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("home: create %s: %w", dir, err)
		}
	}
	return nil
}

// DBPath returns the harness SQLite database path below root.
func DBPath(root string) string {
	return filepath.Join(root, DBFilename)
}

// ProjectRepoPath returns the default memory repo path for slug below root.
func ProjectRepoPath(root, slug string) (string, error) {
	if err := project.ValidateSlug(slug); err != nil {
		return "", fmt.Errorf("home: project repo path: %w", err)
	}
	return filepath.Join(root, "projects", slug), nil
}
