package main

import (
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

// auditFixture creates a temp dir with a single .go file and runs Audit.
func auditFixture(t *testing.T, content string) Report {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/fixture.go", content)
	return Audit(dir, makeAllowlist(nil))
}

// blockedBy returns whether report.Blocked contains a diagnostic with the
// given canonical name.
func blockedBy(r Report, fn string) bool {
	for _, c := range r.Blocked {
		if c.Fn == fn {
			return true
		}
	}
	return false
}

// countBlocked returns how many Blocked diagnostics have the given fn.
func countBlocked(r Report, fn string) int {
	n := 0
	for _, c := range r.Blocked {
		if c.Fn == fn {
			n++
		}
	}
	return n
}

// ---------- ValidateAllowlist ----------

func TestValidateAllowlist_RejectsNeitherJustificationNorPR(t *testing.T) {
	errs := ValidateAllowlist(makeAllowlist([]entry{
		{File: "x.go", Line: 1, Fn: "os.Stat"},
	}))
	if len(errs) == 0 {
		t.Error("entry with neither justification nor pr should be rejected")
	}
}

func TestValidateAllowlist_RejectsBothJustificationAndPR(t *testing.T) {
	errs := ValidateAllowlist(makeAllowlist([]entry{
		{File: "x.go", Line: 1, Fn: "os.Stat", Justification: "reason", PR: "PR 1"},
	}))
	if len(errs) == 0 {
		t.Error("entry with both justification and pr should be rejected")
	}
}

func TestValidateAllowlist_RejectsDuplicate(t *testing.T) {
	errs := ValidateAllowlist(makeAllowlist([]entry{
		{File: "x.go", Line: 7, Fn: "os.Stat", PR: "PR 99"},
		{File: "x.go", Line: 7, Fn: "os.Stat", PR: "PR 99"},
	}))
	if len(errs) == 0 {
		t.Error("duplicate entries should be rejected")
	}
}

func TestValidateAllowlist_AcceptsValidMix(t *testing.T) {
	errs := ValidateAllowlist(makeAllowlist([]entry{
		{File: "a.go", Line: 1, Fn: "os.Stat", Justification: "legit"},
		{File: "b.go", Line: 1, Fn: "os.Open", PR: "PR 3"},
	}))
	if len(errs) != 0 {
		t.Errorf("valid allowlist should be accepted: %v", errs)
	}
}

func TestValidateAllowlist_AcceptsSameLineDifferentCol(t *testing.T) {
	errs := ValidateAllowlist(makeAllowlist([]entry{
		{File: "x.go", Line: 7, Col: 1, Fn: "os.Stat", PR: "PR 99"},
		{File: "x.go", Line: 7, Col: 20, Fn: "os.Stat", PR: "PR 99"},
	}))
	if len(errs) != 0 {
		t.Errorf("same-line different-column entries should be accepted: %v", errs)
	}
}

func TestValidateAllowlist_RejectsSameLineSameCol(t *testing.T) {
	errs := ValidateAllowlist(makeAllowlist([]entry{
		{File: "x.go", Line: 7, Col: 20, Fn: "os.Stat", PR: "PR 99"},
		{File: "x.go", Line: 7, Col: 20, Fn: "os.Stat", PR: "PR 99"},
	}))
	if len(errs) == 0 {
		t.Error("same-line same-column duplicate entries should be rejected")
	}
}

// ---------- Audit: classification ----------

func TestAudit_ImportAliasDetected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/alias.go", `package pkg

import filesystem "os"

func foo() { filesystem.RemoveAll("/tmp/x") }
`)
	al := makeAllowlist([]entry{
		{File: "internal/pkg/alias.go", Line: 5, Fn: "os.RemoveAll", PR: "PR 99"},
	})
	if errs := ValidateAllowlist(al); len(errs) > 0 {
		t.Fatal(errs)
	}
	r := Audit(dir, al)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if len(r.Unclassified) > 0 {
		t.Errorf("alias import not matched: %v", r.Unclassified)
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
	r := Audit(dir, al)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	for _, c := range r.Unclassified {
		if c.Fn == "os.RemoveAll" {
			return
		}
	}
	t.Error("os.RemoveAll on line 6 should not match os.Stat allowlist entry")
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
	r := Audit(dir, al)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if len(r.Stale) == 0 {
		t.Error("expected stale entry for line 99")
	}
}

func TestAudit_FsauditDirExempted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "cmd/fsaudit/fake.go", `package main

import "os"

func foo() { os.WriteFile("/tmp/x", []byte("hello"), 0o644) }
`)
	r := Audit(dir, makeAllowlist(nil))
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if len(r.SourceCalls) != 0 {
		t.Errorf("cmd/fsaudit/ should be exempt, got %v", r.SourceCalls)
	}
}

func TestAudit_TestFilesExcluded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/real_test.go", `package pkg_test

import "os"

func TestFoo(t *testing.T) { os.RemoveAll("/tmp/x") }
`)
	r := Audit(dir, makeAllowlist(nil))
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if len(r.SourceCalls) != 0 {
		t.Errorf("test files should be excluded, got %v", r.SourceCalls)
	}
}

func TestAudit_ParseErrorFailsClosed(t *testing.T) {
	r := auditFixture(t, `package pkg

import "os"

func foo(   // missing closing paren
`)
	if r.Err == nil {
		t.Error("parse error should be returned")
	}
}

func TestAudit_ProductionFileOutsideInternalCmdScanned(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pkg/outside.go", `package pkg

import "os"

func init() { os.RemoveAll("/tmp/x") }
`)
	r := Audit(dir, makeAllowlist(nil))
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if len(r.SourceCalls) == 0 {
		t.Error("files outside internal/ and cmd/ must be scanned")
	}
}

// ---------- Audit: multiplicities ----------

func TestAudit_MoreCallsThanEntriesIsUnclassified(t *testing.T) {
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
	r := Audit(dir, al)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	var n int
	for _, c := range r.Unclassified {
		if c.Fn == "os.Stat" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected one unclassified os.Stat call, got %d", n)
	}
}

func TestAudit_TwoCallsOnSameLineClassifiedByColumn(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/pkg/same_line.go", `package pkg

import "os"

func f() { os.Stat("a"); os.Stat("b") }
`)
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
		t.Fatalf("expected 2 os.Stat calls, got %d (cols=%v)", len(cols), cols)
	}
	al := makeAllowlist([]entry{
		{File: "internal/pkg/same_line.go", Line: 5, Col: cols[0], Fn: "os.Stat", PR: "PR 99"},
		{File: "internal/pkg/same_line.go", Line: 5, Col: cols[1], Fn: "os.Stat", PR: "PR 99"},
	})
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

// ---------- Audit: blocked capability escapes ----------

func TestAudit_DotImportBlocked(t *testing.T) {
	r := auditFixture(t, `package pkg

import . "os"

func foo() { RemoveAll("/tmp/x") }
`)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !blockedBy(r, `import ."os"`) {
		t.Errorf("dot import of os not blocked: %v", r.Blocked)
	}
	// The dot-imported call should still be classified.
	found := false
	for _, c := range r.SourceCalls {
		if c.Fn == "os.RemoveAll" {
			found = true
		}
	}
	if !found {
		t.Errorf("dot-imported call not detected in calls: %v", r.SourceCalls)
	}
}

func TestAudit_DotImportExtractionBlocked(t *testing.T) {
	r := auditFixture(t, `package pkg

import . "os"

var remove = RemoveAll
`)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !blockedBy(r, "os.RemoveAll") {
		t.Errorf("dot-imported extraction not blocked: %v", r.Blocked)
	}
}

func TestAudit_WatchedPolicyIsCompiled(t *testing.T) {
	for _, s := range []string{"OpenFile", "RemoveAll", "MkdirAll", "Truncate", "Link", "Symlink", "CopyFS", "DirFS", "MkdirTemp"} {
		if !slicesContains(watched["os"], s) {
			t.Errorf("os.%s must be in compiled watched policy", s)
		}
	}
	if _, ok := watched["path/filepath"]; !ok {
		t.Error("path/filepath must be in compiled watched policy")
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

// ---------- Audit: os.Root type references outside rootfs ----------

func TestAudit_NonRootfsOsRootBlocked(t *testing.T) {
	tests := []struct {
		name, code string
	}{
		{"pointer param", `package pkg
import "os"
func wipe(root *os.Root) { _ = root }`},
		{"value param", `package pkg
import "os"
func use(root os.Root) {}`},
		{"type alias", `package pkg
import "os"
type MyRoot = os.Root`},
		{"parenthesized alias", `package pkg
import "os"
type P = (os.Root)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := auditFixture(t, tt.code)
			if r.Err != nil {
				t.Fatal(r.Err)
			}
			if !blockedBy(r, "os.Root") {
				t.Errorf("%s: os.Root not blocked: %v", tt.name, r.Blocked)
			}
		})
	}
}

func TestAudit_FunctionValueExtractionBlocked(t *testing.T) {
	tests := []struct {
		name, code string
	}{
		{"return", `package pkg
import "os"
func fn() func(string) error { return os.RemoveAll }`},
		{"composite literal", `package pkg
import "os"
var funcs = []func(string) error{os.RemoveAll}`},
		{"pass as arg", `package pkg
import "os"
func use(fn func(string) error) {}
func f() { use(os.RemoveAll) }`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := auditFixture(t, tt.code)
			if r.Err != nil {
				t.Fatal(r.Err)
			}
			if !blockedBy(r, "os.RemoveAll") {
				t.Errorf("%s: extraction not blocked: %v", tt.name, r.Blocked)
			}
		})
	}
}

func TestAudit_OsRootConversionBlocked(t *testing.T) {
	r := auditFixture(t, `package pkg

import "os"

func f(r *os.Root) {
	_ = os.Root(*r)
}
`)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if n := countBlocked(r, "os.Root"); n != 2 {
		t.Errorf("expected 2 os.Root blocked diagnostics (param + conversion), got %d: %v", n, r.Blocked)
	}
}

// ---------- Audit: rootfs os.Root containment ----------

func TestAudit_RootBackingFieldAccepted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/rootfs/root.go", `package rootfs

import "os"

type Root struct {
	root *os.Root
}
`)
	r := Audit(dir, makeAllowlist(nil))
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if blockedBy(r, "os.Root in rootfs") {
		t.Error("Root.root backing field should be accepted")
	}
}

func TestAudit_RootfsOsRootBlocked(t *testing.T) {
	tests := []struct {
		name, code string
	}{
		{"exported func", `package rootfs
import "os"
func Raw(root *Root) *os.Root { return root.root }`},
		{"unexported func", `package rootfs
import "os"
func helper(root *os.Root) {}`},
		{"type alias", `package rootfs
import "os"
type PublicRoot = os.Root`},
		{"embedded struct", `package rootfs
import "os"
type Leak struct{ *os.Root }`},
		{"local alias passthrough", `package rootfs
import "os"
type raw = os.Root
func Leak(r *Root) *raw { return r.root }`},
		{"parenthesized", `package rootfs
import "os"
type P = (os.Root)`},
		{"generic index", `package rootfs
import "os"
type Box[T any] struct{ v T }
type G = Box[*os.Root]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "internal/rootfs/fixture.go", tt.code)
			r := Audit(dir, makeAllowlist(nil))
			if r.Err != nil {
				t.Fatal(r.Err)
			}
			if !blockedBy(r, "os.Root in rootfs") {
				t.Errorf("%s: os.Root in rootfs not blocked: %v", tt.name, r.Blocked)
			}
		})
	}
}
