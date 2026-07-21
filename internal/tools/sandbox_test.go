package tools

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidatePathRejectsSiblingWithSharedPrefix(t *testing.T) {
	root := t.TempDir()
	sibling := root + "-sibling"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("MkdirAll sibling: %v", err)
	}
	outside := filepath.Join(sibling, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatalf("WriteFile outside: %v", err)
	}

	if _, err := validatePath(outside, []string{root}); !errors.Is(err, ErrSandboxViolation) {
		t.Fatalf("validatePath sibling error = %v, want ErrSandboxViolation", err)
	}
}

func TestValidatePathWindowsCaseInsensitiveMissingPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows path casing")
	}
	root := t.TempDir()
	missing := filepath.Join(root, "missing.txt")

	got, err := validatePath(missing, []string{strings.ToUpper(root)})
	if err != nil {
		t.Fatalf("validatePath mixed-case root: %v", err)
	}
	if got != missing {
		t.Fatalf("validatePath = %q, want %q", got, missing)
	}
}
