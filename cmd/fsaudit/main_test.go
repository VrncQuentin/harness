package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// ---------- ValidateAllowlist ----------

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

func TestValidateAllowlist_AcceptsSameLineDifferentCol(t *testing.T) {
	al := makeAllowlist([]entry{
		{File: "x.go", Line: 7, Col: 1, Fn: "os.Stat", PR: "PR 99"},
		{File: "x.go", Line: 7, Col: 20, Fn: "os.Stat", PR: "PR 99"},
	})
	errs := ValidateAllowlist(al)
	if len(errs) != 0 {
		t.Errorf("same-line different-column entries should be accepted: %v", errs)
	}
}

func TestValidateAllowlist_RejectsSameLineSameCol(t *testing.T) {
	al := makeAllowlist([]entry{
		{File: "x.go", Line: 7, Col: 20, Fn: "os.Stat", PR: "PR 99"},
		{File: "x.go", Line: 7, Col: 20, Fn: "os.Stat", PR: "PR 99"},
	})
	errs := ValidateAllowlist(al)
	if len(errs) == 0 {
		t.Error("same-line same-column duplicate entries should be rejected")
	}
}

// ---------- Audit: basic classification ----------

func TestAudit_ImportAliasDetected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/alias.go", `package pkg

import filesystem "os"

func foo() { filesystem.RemoveAll("/tmp/x") }
`)
	al := makeAllowlist([]entry{
		{File: "internal/pkg/alias.go", Line: 5, Fn: "os.RemoveAll", PR: "PR 99"},
	})
	report := Audit(dir, al)
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	if len(report.Unclassified) > 0 {
		t.Errorf("alias import not matched: %v", report.Unclassified)
	}
}

func TestAudit_WrongSymbolAtAllowedLineRejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/bad.go", `package pkg

import "os"

func foo() {
	os.RemoveAll("/tmp/x")
	os.Stat("/tmp/y")
}
`)
	al := makeAllowlist([]entry{
		{File: "internal/pkg/bad.go", Line: 6, Fn: "os.Stat", PR: "PR 99"},
	})
	report := Audit(dir, al)
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	found := false
	for _, c := range report.Unclassified {
		if c.Fn == "os.RemoveAll" {
			found = true
		}
	}
	if !found {
		t.Error("os.RemoveAll on line 6 should not match os.Stat allowlist entry")
	}
}

func TestAudit_StaleEntryDetected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/real.go", `package pkg

import "os"

func foo() { os.Stat("/tmp/x") }
`)
	al := makeAllowlist([]entry{
		{File: "internal/pkg/real.go", Line: 5, Fn: "os.Stat", PR: "PR 99"},
		{File: "internal/pkg/real.go", Line: 99, Fn: "os.Stat", PR: "PR 99"},
	})
	report := Audit(dir, al)
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	if len(report.Stale) == 0 {
		t.Error("expected stale entry for line 99")
	}
}

func TestAudit_SecurityPackageScanned(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/rootfs/fake.go", `package rootfs

import "os"

func FakeOpen() (*os.Root, error) { return os.OpenRoot("/tmp/fake") }
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	if len(report.SourceCalls) == 0 {
		t.Error("rootfs should be scanned by the audit")
	}
}

func TestAudit_FsauditDirExempted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "cmd/fsaudit/fake.go", `package main

import "os"

func foo() { os.WriteFile("/tmp/x", []byte("hello"), 0o644) }
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	if len(report.SourceCalls) != 0 {
		t.Errorf("cmd/fsaudit/ should be exempt, got %v", report.SourceCalls)
	}
}

func TestAudit_TestFilesExcluded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/real_test.go", `package pkg_test

import "os"

func TestFoo(t *testing.T) { os.RemoveAll("/tmp/x") }
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	if len(report.SourceCalls) != 0 {
		t.Errorf("test files should be excluded, got %v", report.SourceCalls)
	}
}

// ---------- Audit: multiplicities ----------

func TestAudit_MoreSourceCallsThanEntriesIsUnclassified(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/dup.go", `package pkg

import "os"

func foo() {
	os.Stat("/tmp/a")
	os.Stat("/tmp/b")
}
`)
	al := makeAllowlist([]entry{
		{File: "internal/pkg/dup.go", Line: 6, Fn: "os.Stat", PR: "PR 99"},
	})
	report := Audit(dir, al)
	if report.Err != nil {
		t.Fatal(report.Err)
	}
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

func TestAudit_TwoCallsOnSameLineClassifiedByColumn(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/same_line.go", `package pkg

import "os"

func f() { os.Stat("a"); os.Stat("b") }
`)
	// First run without allowlist to discover actual column positions.
	report1 := Audit(dir, makeAllowlist(nil))
	if report1.Err != nil {
		t.Fatal(report1.Err)
	}
	var cols []int
	for _, c := range report1.SourceCalls {
		if c.Fn == "os.Stat" {
			cols = append(cols, c.Col)
		}
	}
	if len(cols) != 2 {
		t.Fatalf("expected 2 os.Stat calls on same line, got %d (cols=%v)", len(cols), cols)
	}

	al := makeAllowlist([]entry{
		{File: "internal/pkg/same_line.go", Line: 5, Col: cols[0], Fn: "os.Stat", PR: "PR 99"},
		{File: "internal/pkg/same_line.go", Line: 5, Col: cols[1], Fn: "os.Stat", PR: "PR 99"},
	})

	// Must validate first — this is what main does.
	if errs := ValidateAllowlist(al); len(errs) > 0 {
		t.Fatalf("validation failed: %v", errs)
	}

	report2 := Audit(dir, al)
	if report2.Err != nil {
		t.Fatal(report2.Err)
	}
	if len(report2.Unclassified) > 0 {
		t.Errorf("both calls should be classified: unclass=%v", report2.Unclassified)
	}
	if len(report2.Stale) > 0 {
		t.Errorf("no stale entries expected: %v", report2.Stale)
	}
}

// ---------- Audit: parse error propagation ----------

func TestAudit_ParseErrorFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/broken.go", `package pkg

import "os"

func foo(   // missing closing paren
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err == nil {
		t.Error("parse error should be returned")
	}
}

// ---------- Audit: blocked capability escapes ----------

func TestAudit_DotImportCallDetected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/dotimp.go", `package pkg

import . "os"

func foo() { RemoveAll("/tmp/x") }
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	// The dot import itself must be blocked.
	blockedImport := false
	for _, c := range report.Blocked {
		if strings.Contains(c.Fn, `"os"`) {
			blockedImport = true
		}
	}
	if !blockedImport {
		t.Errorf("dot import of os not blocked, blocked=%v", report.Blocked)
	}
	// The dot-imported call should still be classified.
	found := false
	for _, c := range report.SourceCalls {
		if c.Fn == "os.RemoveAll" {
			found = true
		}
	}
	if !found {
		t.Errorf("dot-imported function call not detected in calls, calls=%v", report.SourceCalls)
	}
}

func TestAudit_DotImportExtractionBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/dotimp2.go", `package pkg

import . "os"

var remove = RemoveAll
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.RemoveAll" {
			found = true
		}
	}
	if !found {
		t.Errorf("dot-imported extraction not blocked: %v", report.Blocked)
	}
}

func TestAudit_PassWatchedFunctionAsArgBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/val.go", `package pkg

import "os"

func use(os.RemoveAll)
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.RemoveAll" {
			found = true
		}
	}
	if !found {
		t.Errorf("passing os.RemoveAll as arg not blocked: %v", report.Blocked)
	}
}

func TestAudit_ReturnWatchedFunctionBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/ret.go", `package pkg

import "os"

func fn() func(string) error { return os.RemoveAll }
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.RemoveAll" {
			found = true
		}
	}
	if !found {
		t.Errorf("returning os.RemoveAll not blocked: %v", report.Blocked)
	}
}

func TestAudit_CompositeLiteralWatchedFunctionBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/comp.go", `package pkg

import "os"

var funcs = []func(string) error{os.RemoveAll}
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.RemoveAll" {
			found = true
		}
	}
	if !found {
		t.Errorf("composite-literal os.RemoveAll not blocked: %v", report.Blocked)
	}
}

func TestAudit_OsRootTypeRefBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/roottype.go", `package pkg

import "os"

func wipe(root *os.Root) { _ = root }
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.Root" {
			found = true
		}
	}
	if !found {
		t.Errorf("*os.Root type reference not blocked: %v", report.Blocked)
	}
}

func TestAudit_ValueOsRootBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/valroot.go", `package pkg

import "os"

func use(root os.Root) {}
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.Root" {
			found = true
		}
	}
	if !found {
		t.Errorf("value os.Root type reference not blocked: %v", report.Blocked)
	}
}

func TestAudit_NonRootfsParenthesizedOsRootBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/paren.go", `package pkg

import "os"

type P = (os.Root)
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.Root" {
			found = true
		}
	}
	if !found {
		t.Errorf("parenthesized os.Root outside rootfs not blocked: %v", report.Blocked)
	}
}

func TestAudit_OsRootTypeSpecBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/rootalias.go", `package pkg

import "os"

type MyRoot = os.Root
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.Root" {
			found = true
		}
	}
	if !found {
		t.Errorf("os.Root type alias not blocked: %v", report.Blocked)
	}
}

func TestAudit_RootfsBackingFieldAccepted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/rootfs/root.go", `package rootfs

import "os"

type Root struct {
	root *os.Root
}
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	for _, c := range report.Blocked {
		if c.Fn == "os.Root in rootfs" {
			t.Errorf("Root.root backing field should be accepted, got blocked: %v", c)
		}
	}
}

func TestAudit_RootfsUnexportedOsRootBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/rootfs/bad.go", `package rootfs

import "os"

func helper(root *os.Root) {}
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.Root in rootfs" {
			found = true
		}
	}
	if !found {
		t.Errorf("unexported *os.Root signature in rootfs should be blocked: %v", report.Blocked)
	}
}

func TestAudit_RootfsParenthesizedBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/rootfs/paren.go", `package rootfs

import "os"

type P = (os.Root)
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.Root in rootfs" {
			found = true
		}
	}
	if !found {
		t.Errorf("parenthesized os.Root in rootfs should be blocked: %v", report.Blocked)
	}
}

func TestAudit_RootfsGenericIndexBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/rootfs/generic.go", `package rootfs

import "os"

type Box[T any] struct{ v T }

type G = Box[*os.Root]
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.Root in rootfs" {
			found = true
		}
	}
	if !found {
		t.Errorf("*os.Root inside generic index in rootfs should be blocked: %v", report.Blocked)
	}
}

func TestAudit_RootfsExportedOsRootBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/rootfs/leak.go", `package rootfs

import "os"

func Raw(root *Root) *os.Root { return root.root }
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.Root in rootfs" {
			found = true
		}
	}
	if !found {
		t.Errorf("exported os.Root from rootfs not blocked: %v", report.Blocked)
	}
}

func TestAudit_RootfsExportedTypeAliasBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/rootfs/alias.go", `package rootfs

import "os"

type PublicRoot = os.Root
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.Root in rootfs" {
			found = true
		}
	}
	if !found {
		t.Errorf("exported os.Root type alias from rootfs not blocked: %v", report.Blocked)
	}
}

func TestAudit_RootfsEmbeddedOsRootBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/rootfs/embed.go", `package rootfs

import "os"

type Leak struct{ *os.Root }
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.Root in rootfs" {
			found = true
		}
	}
	if !found {
		t.Errorf("embedded *os.Root in rootfs struct not blocked: %v", report.Blocked)
	}
}

func TestAudit_RootfsAliasThroughLocalTypeBlocked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/rootfs/alias2.go", `package rootfs

import "os"

type raw = os.Root

func Leak(r *Root) *raw { return r.root }
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	found := false
	for _, c := range report.Blocked {
		if c.Fn == "os.Root in rootfs" {
			found = true
		}
	}
	if !found {
		t.Errorf("alias-through-local-type os.Root exposure not blocked: %v", report.Blocked)
	}
}

func TestAudit_ProductionFileOutsideInternalCmdScanned(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pkg/outside.go", `package pkg

import "os"

func init() { os.RemoveAll("/tmp/x") }
`)
	report := Audit(dir, makeAllowlist(nil))
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	if len(report.SourceCalls) == 0 {
		t.Error("files outside internal/ and cmd/ must be scanned")
	}
}

// ---------- Audit: watched policy immutability ----------

func TestAudit_WatchedPolicyIsCompiled(t *testing.T) {
	if _, ok := watched["os"]; !ok {
		t.Error("os package must be in compiled watched policy")
	}
	if _, ok := watched["path/filepath"]; !ok {
		t.Error("path/filepath package must be in compiled watched policy")
	}
	for _, s := range []string{"OpenFile", "RemoveAll", "MkdirAll", "Truncate", "Link", "Symlink", "CopyFS", "DirFS", "MkdirTemp"} {
		if !slicesContains(watched["os"], s) {
			t.Errorf("os.%s must be in compiled watched policy", s)
		}
	}
}

func slicesContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ---------- JSON round-trip ----------

func TestAllowlistJSON_RoundTrip(t *testing.T) {
	al := allowlist{
		Perm: []entry{
			{File: "x.go", Line: 1, Col: 0, Fn: "os.Stat", Justification: "legit"},
		},
		Migr: []entry{
			{File: "y.go", Line: 2, Col: 14, Fn: "os.Open", PR: "PR 3"},
		},
	}
	data, err := json.MarshalIndent(al, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var parsed allowlist
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Perm) != 1 || parsed.Perm[0].Fn != "os.Stat" {
		t.Error("permanent entry lost in round-trip")
	}
	if len(parsed.Migr) != 1 || parsed.Migr[0].Fn != "os.Open" {
		t.Error("migration entry lost in round-trip")
	}
	if parsed.Migr[0].Col != 14 {
		t.Errorf("expected col 14, got %d", parsed.Migr[0].Col)
	}
}
