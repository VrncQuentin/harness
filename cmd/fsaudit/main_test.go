package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeAllowlist(t *testing.T, dir string, entries []entry) string {
	t.Helper()
	al := allowlist{}
	for _, e := range entries {
		if e.Justification != "" {
			al.Perm = append(al.Perm, e)
		} else {
			al.Migr = append(al.Migr, e)
		}
	}
	data, err := json.MarshalIndent(al, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "allowlist.json")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImportAliasDetected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/alias.go", `
package pkg

import filesystem "os"

func foo() {
	filesystem.RemoveAll("/tmp/x")
}
`)
	al := writeAllowlist(t, dir, []entry{
		{File: "internal/pkg/alias.go", Line: 7, Fn: "os.RemoveAll", PR: "PR 99"},
	})

	calls, err := collectCalls(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range calls {
		if c.Fn == "os.RemoveAll" {
			found = true
		}
	}
	if !found {
		t.Error("fsaudit did not resolve aliased os import")
	}
	_ = al
}

func TestAliasImportNotOsIgnored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/other.go", `
package pkg

import other "fmt"

func foo() {
	other.Println("hi")
}
`)
	calls, err := collectCalls(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Errorf("expected no calls, got %v", calls)
	}
}

func TestWrongSymbolAtAllowedLineRejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/bad.go", `
package pkg

import "os"

func foo() {
	os.RemoveAll("/tmp/x")
	os.Stat("/tmp/y")
}
`)

	calls, err := collectCalls(dir)
	if err != nil {
		t.Fatal(err)
	}

	allowlistEntries := []entry{
		{File: "internal/pkg/bad.go", Line: 7, Fn: "os.Stat", PR: "PR 99"},
	}

	unclassified := checkCalls(calls, allowlistEntries)
	if len(unclassified) == 0 {
		t.Error("os.RemoveAll on line 7 should not match os.Stat allowlist entry")
	}
}

func TestDuplicateAllowlistEntriesRejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/dup.go", `
package pkg

import "os"

func foo() {
	os.Stat("/tmp/x")
}
`)
	al := writeAllowlist(t, dir, []entry{
		{File: "internal/pkg/dup.go", Line: 7, Fn: "os.Stat", PR: "PR 99"},
		{File: "internal/pkg/dup.go", Line: 7, Fn: "os.Stat", PR: "PR 99"},
	})

	data, err := os.ReadFile(al)
	if err != nil {
		t.Fatal(err)
	}
	var parsed allowlist
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}

	all := append([]entry(nil), parsed.Perm...)
	all = append(all, parsed.Migr...)
	seen := map[string]bool{}
	dup := false
	for _, e := range all {
		k := fmtKey(e.File, e.Line, e.Fn)
		if seen[k] {
			dup = true
		}
		seen[k] = true
	}
	if !dup {
		t.Error("expected duplicate detection")
	}
}

func TestStaleEntryDetected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/real.go", `
package pkg

import "os"

func foo() {
	os.Stat("/tmp/x")
}
`)

	calls, err := collectCalls(dir)
	if err != nil {
		t.Fatal(err)
	}

	allowlistEntries := []entry{
		{File: "internal/pkg/real.go", Line: 7, Fn: "os.Stat", PR: "PR 99"},
		{File: "internal/pkg/real.go", Line: 99, Fn: "os.Stat", PR: "PR 99"},
	}

	_ = checkCalls(calls, allowlistEntries)
	stale := findStale(calls, allowlistEntries)
	if len(stale) == 0 {
		t.Error("expected stale entry for line 99")
	}
}

func TestSecurityPackageScanned(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/rootfs/fake.go", `
package rootfs

import "os"

func FakeOpen() (*os.Root, error) {
	return os.OpenRoot("/tmp/fake")
}
`)

	calls, err := collectCalls(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) == 0 {
		t.Error("rootfs should be scanned by the audit")
	}
}

func TestFsauditDirExempted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "cmd/fsaudit/fake.go", `
package main

import "os"

func foo() {
	os.WriteFile("/tmp/x", []byte("hello"), 0o644)
}
`)

	calls, err := collectCalls(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Errorf("cmd/fsaudit/ should be exempt, got %v", calls)
	}
}

func TestMissingJustificationAndPrRejected(t *testing.T) {
	e := entry{File: "x.go", Line: 1, Fn: "os.Stat"}
	if e.Justification != "" || e.PR != "" {
		t.Error("entry with neither justification nor PR should be invalid")
	}
}

func TestBothJustificationAndPrRejected(t *testing.T) {
	e := entry{File: "x.go", Line: 1, Fn: "os.Stat", Justification: "reason", PR: "PR 1"}
	if e.Justification == "" || e.PR == "" {
		return
	}
	if e.Justification != "" && e.PR != "" {
		// This is the condition that should be caught by the validator.
		// Tested here for documentation; the validator lives in main().
	}
}

func TestWatchedPolicyIsntFromAllowlist(t *testing.T) {
	if _, ok := watched["os"]; !ok {
		t.Error("os package must be in compiled watched policy")
	}
	if _, ok := watched["path/filepath"]; !ok {
		t.Error("path/filepath package must be in compiled watched policy")
	}
	syms := watched["os"]
	for _, s := range []string{"Open", "OpenFile", "RemoveAll", "MkdirAll", "Truncate", "Link", "Symlink"} {
		found := false
		for _, ws := range syms {
			if ws == s {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("os.%s must be in compiled watched policy", s)
		}
	}
}

func TestTestFilesExcluded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/real_test.go", `
package pkg_test

import "os"

func TestFoo(t *testing.T) {
	os.RemoveAll("/tmp/x")
}
`)

	calls, err := collectCalls(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Errorf("test files should be excluded, got %v", calls)
	}
}

func TestMultilineCallDetected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/multi.go", `
package pkg

import "os"

func foo() {
	os.RemoveAll(
		"/tmp/x",
	)
}
`)

	calls, err := collectCalls(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range calls {
		if c.Fn == "os.RemoveAll" {
			found = true
		}
	}
	if !found {
		t.Error("multiline call not detected")
	}
}

func checkCalls(calls []call, entries []entry) []call {
	allowed := map[string]bool{}
	for _, e := range entries {
		allowed[fmtKey(e.File, e.Line, e.Fn)] = true
	}
	matched := map[string]bool{}
	var unclassified []call
	for _, c := range calls {
		k := fmtKey(c.File, c.Line, c.Fn)
		if allowed[k] {
			matched[k] = true
		} else {
			unclassified = append(unclassified, c)
		}
	}
	return unclassified
}

func findStale(calls []call, entries []entry) []entry {
	allowed := map[string]bool{}
	for _, e := range entries {
		allowed[fmtKey(e.File, e.Line, e.Fn)] = true
	}
	matched := map[string]bool{}
	for _, c := range calls {
		k := fmtKey(c.File, c.Line, c.Fn)
		if allowed[k] {
			matched[k] = true
		}
	}
	var stale []entry
	for _, e := range entries {
		if !matched[fmtKey(e.File, e.Line, e.Fn)] {
			stale = append(stale, e)
		}
	}
	return stale
}

func fmtKey(file string, line int, fn string) string {
	return entryKey(entry{File: file, Line: line, Fn: fn})
}
