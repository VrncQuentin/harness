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

func makeAllowlist(entries []entry) allowlist {
	var al allowlist
	for _, e := range entries {
		if e.Justification != "" {
			al.Perm = append(al.Perm, e)
		} else {
			al.Migr = append(al.Migr, e)
		}
	}
	return al
}

func mustMarshalAllowlist(t *testing.T, al allowlist) string {
	t.Helper()
	data, err := json.MarshalIndent(al, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// ---------- ValidateAllowlist tests ----------

func TestValidateAllowlist_RejectsNeitherJustificationNorPR(t *testing.T) {
	al := makeAllowlist([]entry{
		{File: "x.go", Line: 1, Fn: "os.Stat"},
	})
	errs := ValidateAllowlist(al)
	if len(errs) == 0 {
		t.Error("entry with neither justification nor pr should be rejected")
	}
}

func TestValidateAllowlist_RejectsBothJustificationAndPR(t *testing.T) {
	al := makeAllowlist([]entry{
		{File: "x.go", Line: 1, Fn: "os.Stat", Justification: "reason", PR: "PR 1"},
	})
	errs := ValidateAllowlist(al)
	if len(errs) == 0 {
		t.Error("entry with both justification and pr should be rejected")
	}
}

func TestValidateAllowlist_RejectsDuplicate(t *testing.T) {
	al := makeAllowlist([]entry{
		{File: "x.go", Line: 7, Fn: "os.Stat", PR: "PR 99"},
		{File: "x.go", Line: 7, Fn: "os.Stat", PR: "PR 99"},
	})
	errs := ValidateAllowlist(al)
	if len(errs) == 0 {
		t.Error("duplicate entries should be rejected")
	}
}

func TestValidateAllowlist_AcceptsValidMix(t *testing.T) {
	al := makeAllowlist([]entry{
		{File: "a.go", Line: 1, Fn: "os.Stat", Justification: "legit"},
		{File: "b.go", Line: 1, Fn: "os.Open", PR: "PR 3"},
	})
	errs := ValidateAllowlist(al)
	if len(errs) != 0 {
		t.Errorf("valid allowlist should be accepted: %v", errs)
	}
}

func TestValidateAllowlist_RequiresExactConfigKeys(t *testing.T) {
	data := mustMarshalAllowlist(t, allowlist{
		Perm: []entry{{File: "x.go", Line: 1, Fn: "os.Stat"}},
	})
	var parsed allowlist
	if err := json.Unmarshal([]byte(data), &parsed); err != nil {
		t.Fatal(err)
	}
	// An entry with no justification and no pr should be accepted by
	// the JSON parser but rejected by ValidateAllowlist.
	errs := ValidateAllowlist(parsed)
	if len(errs) == 0 {
		t.Error("validation should reject entry with no classification fields")
	}
}

// ---------- Audit tests ----------

func TestAudit_ImportAliasDetected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/alias.go", `
package pkg

import filesystem "os"

func foo() {
	filesystem.RemoveAll("/tmp/x")
}
`)
	al := makeAllowlist([]entry{
		{File: "internal/pkg/alias.go", Line: 7, Fn: "os.RemoveAll", PR: "PR 99"},
	})
	report := Audit(dir, al)
	if len(report.Unclassified) > 0 {
		t.Errorf("alias import not matched: %v", report.Unclassified)
	}
	if len(report.Stale) > 0 {
		t.Errorf("stale entries: %v", report.Stale)
	}
}

func TestAudit_AliasImportNotOsIgnored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/other.go", `
package pkg

import other "fmt"

func foo() {
	other.Println("hi")
}
`)
	report := Audit(dir, makeAllowlist(nil))
	if len(report.SourceCalls) != 0 {
		t.Errorf("non-watched alias calls should be ignored, got %v", report.SourceCalls)
	}
}

func TestAudit_WrongSymbolAtAllowedLineRejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/bad.go", `
package pkg

import "os"

func foo() {
	os.RemoveAll("/tmp/x")
	os.Stat("/tmp/y")
}
`)
	al := makeAllowlist([]entry{
		{File: "internal/pkg/bad.go", Line: 7, Fn: "os.Stat", PR: "PR 99"},
	})
	report := Audit(dir, al)
	found := false
	for _, c := range report.Unclassified {
		if c.Fn == "os.RemoveAll" {
			found = true
		}
	}
	if !found {
		t.Error("os.RemoveAll on line 7 should not match os.Stat allowlist entry")
	}
}

func TestAudit_StaleEntryDetected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/real.go", `
package pkg

import "os"

func foo() {
	os.Stat("/tmp/x")
}
`)
	al := makeAllowlist([]entry{
		{File: "internal/pkg/real.go", Line: 7, Fn: "os.Stat", PR: "PR 99"},
		{File: "internal/pkg/real.go", Line: 99, Fn: "os.Stat", PR: "PR 99"},
	})
	report := Audit(dir, al)
	if len(report.Stale) == 0 {
		t.Error("expected stale entry for line 99")
	}
}

func TestAudit_SecurityPackageScanned(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/rootfs/fake.go", `
package rootfs

import "os"

func FakeOpen() (*os.Root, error) {
	return os.OpenRoot("/tmp/fake")
}
`)
	report := Audit(dir, makeAllowlist(nil))
	if len(report.SourceCalls) == 0 {
		t.Error("rootfs should be scanned by the audit")
	}
}

func TestAudit_FsauditDirExempted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "cmd/fsaudit/fake.go", `
package main

import "os"

func foo() {
	os.WriteFile("/tmp/x", []byte("hello"), 0o644)
}
`)
	report := Audit(dir, makeAllowlist(nil))
	if len(report.SourceCalls) != 0 {
		t.Errorf("cmd/fsaudit/ should be exempt, got %v", report.SourceCalls)
	}
}

func TestAudit_TestFilesExcluded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/real_test.go", `
package pkg_test

import "os"

func TestFoo(t *testing.T) {
	os.RemoveAll("/tmp/x")
}
`)
	report := Audit(dir, makeAllowlist(nil))
	if len(report.SourceCalls) != 0 {
		t.Errorf("test files should be excluded, got %v", report.SourceCalls)
	}
}

func TestAudit_MultilineCallDetected(t *testing.T) {
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
	report := Audit(dir, makeAllowlist(nil))
	found := false
	for _, c := range report.SourceCalls {
		if c.Fn == "os.RemoveAll" {
			found = true
		}
	}
	if !found {
		t.Error("multiline call not detected")
	}
}

// ---------- multiplicity tests ----------

func TestAudit_DuplicateCallsMatchDuplicateAllowlist(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/dup.go", `
package pkg

import "os"

func foo() {
	os.Stat("/tmp/a")
	os.Stat("/tmp/b")
}
`)
	al := makeAllowlist([]entry{
		{File: "internal/pkg/dup.go", Line: 7, Fn: "os.Stat", PR: "PR 99"},
		{File: "internal/pkg/dup.go", Line: 8, Fn: "os.Stat", PR: "PR 99"},
	})
	report := Audit(dir, al)
	if len(report.Unclassified) > 0 {
		t.Errorf("matching multiplicities should pass: %v", report.Unclassified)
	}
	if len(report.Stale) > 0 {
		t.Errorf("no stale entries expected: %v", report.Stale)
	}
}

func TestAudit_MoreSourceCallsThanEntriesIsUnclassified(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/dup.go", `
package pkg

import "os"

func foo() {
	os.Stat("/tmp/a")
	os.Stat("/tmp/b")
}
`)
	al := makeAllowlist([]entry{
		{File: "internal/pkg/dup.go", Line: 7, Fn: "os.Stat", PR: "PR 99"},
	})
	report := Audit(dir, al)
	// The two calls on lines 7 and 8 match one allowlist entry (line 7).
	// The call on line 8 should be unclassified (different line means different entry key).
	osStatCalls := 0
	for _, c := range report.Unclassified {
		if c.Fn == "os.Stat" {
			osStatCalls++
		}
	}
	if osStatCalls != 1 {
		t.Errorf("expected one unclassified os.Stat call, got %d", osStatCalls)
	}
}

func TestAudit_TwoIdenticalCallsOnSameLineNeedTwoEntries(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/same_line.go", `package pkg

import "os"

func foo() { os.Stat("/a"); os.Stat("/b") }
`)
	al := makeAllowlist([]entry{
		{File: "internal/pkg/same_line.go", Line: 5, Fn: "os.Stat", PR: "PR 99"},
	})
	report := Audit(dir, al)
	// One remaining call on line 7 should be stale-then-unclassified.
	osStatUnclass := 0
	for _, c := range report.Unclassified {
		if c.Fn == "os.Stat" {
			osStatUnclass++
		}
	}
	if osStatUnclass != 1 {
		t.Errorf("expected one unclassified os.Stat for same-line multiplicity, got %d (blocked=%d)", osStatUnclass, len(report.Blocked))
	}
}

// ---------- dot-import tests ----------

func TestAudit_DotImportDetected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/dotimp.go", `
package pkg

import . "os"

func foo() {
	RemoveAll("/tmp/x")
}
`)
	report := Audit(dir, makeAllowlist(nil))
	found := false
	for _, c := range report.SourceCalls {
		if c.Fn == "os.RemoveAll" {
			found = true
		}
	}
	if !found {
		t.Errorf("dot-imported function call not detected, calls=%v", report.SourceCalls)
	}
}

func TestAudit_DotImportNotWatchedIgnored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/dotimp.go", `
package pkg

import . "fmt"

func foo() {
	Println("hi")
}
`)
	report := Audit(dir, makeAllowlist(nil))
	if len(report.SourceCalls) != 0 {
		t.Errorf("non-watched dot import should be ignored, got %v", report.SourceCalls)
	}
}

// ---------- function-value extraction tests ----------

func TestAudit_FunctionValueExtractionBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/val.go", `
package pkg

import "os"

var fn = os.RemoveAll
`)
	report := Audit(dir, makeAllowlist(nil))
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.RemoveAll" {
			found = true
		}
	}
	if !found {
		t.Errorf("function value extraction not blocked, blocked=%v", report.Blocked)
	}
}

func TestAudit_FunctionValueShortDeclBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/val2.go", `
package pkg

import "os"

func foo() {
	rm := os.RemoveAll
	_ = rm
}
`)
	report := Audit(dir, makeAllowlist(nil))
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.RemoveAll" {
			found = true
		}
	}
	if !found {
		t.Errorf("short-decl function value extraction not blocked, blocked=%v", report.Blocked)
	}
}

// ---------- watched policy immutability ----------

func TestAudit_WatchedPolicyIsntFromAllowlist(t *testing.T) {
	if _, ok := watched["os"]; !ok {
		t.Error("os package must be in compiled watched policy")
	}
	if _, ok := watched["path/filepath"]; !ok {
		t.Error("path/filepath package must be in compiled watched policy")
	}
	syms := watched["os"]
	for _, s := range []string{"Open", "OpenFile", "RemoveAll", "MkdirAll", "Truncate", "Link", "Symlink", "CopyFS", "DirFS", "MkdirTemp"} {
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
