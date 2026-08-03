package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// The spill directory lives outside every sandbox root, so resolution cannot go
// through validatePath. That makes the id the only barrier between a crafted
// handle and an arbitrary file, and it has to hold on its own.
func TestResolveToolout_RejectsAnythingButHex(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name    string
		locator string
	}{
		{name: "parent traversal", locator: TooloutScheme + ".."},
		{name: "traversal with separator", locator: TooloutScheme + "../../etc/passwd"},
		{name: "windows separator", locator: TooloutScheme + `..\..\secrets`},
		{name: "absolute unix path", locator: TooloutScheme + "/etc/passwd"},
		{name: "windows absolute path", locator: TooloutScheme + `C:\Windows\System32\config\SAM`},
		{name: "nested path", locator: TooloutScheme + "sub/dir"},
		{name: "dot", locator: TooloutScheme + "."},
		{name: "uppercase hex", locator: TooloutScheme + "ABCDEF"},
		{name: "non-hex letters", locator: TooloutScheme + "zzzz"},
		{name: "hex with dash", locator: TooloutScheme + "ab-cd"},
		{name: "hex with null byte", locator: TooloutScheme + "abcd\x00ef"},
		{name: "empty id", locator: TooloutScheme},
		{name: "over-long id", locator: TooloutScheme + strings.Repeat("a", tooloutIDMaxLen+1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveToolout(dir, tt.locator)
			if err == nil {
				t.Fatalf("resolved %q to %q, want a rejection", tt.locator, got)
			}
			if got != "" {
				t.Errorf("returned id %q alongside an error", got)
			}
		})
	}
}

func TestResolveToolout_AcceptsGeneratedIDs(t *testing.T) {
	dir := t.TempDir()
	// The shape B3 emits: 16 lowercase hex characters.
	const id = "1a2b3c4d5e6f7890"

	got, err := resolveToolout(dir, TooloutScheme+id)
	if err != nil {
		t.Fatalf("resolveToolout: %v", err)
	}
	// The bare id, for opening relative to the directory handle rather than
	// joining onto a path.
	if got != id {
		t.Errorf("got %q, want %q", got, id)
	}
}

func TestResolveToolout_UnconfiguredDirectoryRefuses(t *testing.T) {
	_, err := resolveToolout("", TooloutScheme+"abcd")
	if !errors.Is(err, ErrTooloutUnavailable) {
		t.Errorf("err = %v, want ErrTooloutUnavailable", err)
	}
}

// writeSpill puts content in the spill directory under the id B3 would use.
func writeSpill(t *testing.T, dir, id, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// B3 emitted a handle no tool resolved, so the preserved output could not be
// reached at all. read now serves it.
func TestRead_ResolvesTooloutHandle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "toolout")
	const id = "00ff11ee22dd33cc"
	writeSpill(t, dir, id, "line one\nline two\nline three\n")

	ci := CallInfo{SandboxRoots: []string{t.TempDir()}, TooloutDir: dir}
	res := (&readTool{}).Execute(context.Background(), ci,
		map[string]any{"locator": TooloutScheme + id})

	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "line two") {
		t.Errorf("spilled output not returned:\n%s", res.Content)
	}
}

func TestRead_TooloutMissingFile(t *testing.T) {
	ci := CallInfo{SandboxRoots: []string{t.TempDir()}, TooloutDir: t.TempDir()}
	res := (&readTool{}).Execute(context.Background(), ci,
		map[string]any{"locator": TooloutScheme + "deadbeefdeadbeef"})

	if res.Error == "" {
		t.Fatal("expected an error for a handle with no file")
	}
	if !strings.Contains(res.Error, "no longer exists") {
		t.Errorf("error = %q, want it to explain the spill is cached", res.Error)
	}
}

// A path-shaped locator must still behave as before.
func TestRead_OrdinaryLocatorUnaffected(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "f.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ci := CallInfo{SandboxRoots: []string{root}}
	res := (&readTool{}).Execute(context.Background(), ci,
		map[string]any{"locator": FormatLocator(path, 2, 3)})

	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if res.Content != "two\nthree\n" {
		t.Errorf("Content = %q", res.Content)
	}
}

// A lexically perfect id says nothing about where the file it names actually
// is. A link inside the spill directory called deadbeefdeadbeef would be
// followed straight out of it, disclosing its target — the same physical-path
// class the sandbox fix closed for project files.
func TestRead_TooloutRefusesLinkedLeaf(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "toolout")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const secret = "SECRET-OUTSIDE-THE-SPILL-DIRECTORY"

	t.Run("linked directory", func(t *testing.T) {
		secretFile := filepath.Join(outside, "sub", "cafebabecafebabe")
		if err := os.MkdirAll(filepath.Dir(secretFile), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(secretFile, []byte(secret), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		linkedDir := filepath.Join(dir, "sub")
		mustLinkDir(t, filepath.Dir(secretFile), linkedDir)
		defer func() { _ = os.Remove(linkedDir) }()

		// The id itself is refused for containing a separator, but assert on
		// the disclosure rather than the reason.
		res := (&readTool{}).Execute(context.Background(),
			CallInfo{SandboxRoots: []string{t.TempDir()}, TooloutDir: dir},
			map[string]any{"locator": TooloutScheme + "sub/cafebabecafebabe"})
		if strings.Contains(res.Content, secret) {
			t.Errorf("disclosed content from outside the spill directory:\n%s", res.Content)
		}
	})

	// A directory link with a perfectly valid hex name. The lexical check has
	// nothing to object to, so only the physical containment check can refuse
	// it — and unlike a file symlink this needs no privilege on Windows, so it
	// exercises that check on both platforms.
	t.Run("linked leaf directory", func(t *testing.T) {
		const id = "abcdefabcdef0123"
		link := filepath.Join(dir, id)
		mustLinkDir(t, outside, link)
		defer func() { _ = os.Remove(link) }()

		data, err := openToolout(dir, TooloutScheme+id)
		if err == nil {
			t.Fatalf("a leaf resolving outside the spill directory was accepted (read %q)", data)
		}
		// os.Root supplies the refusal; the wording is its own.
		if !strings.Contains(err.Error(), "escapes") {
			t.Errorf("err = %v, want a containment refusal", err)
		}
	})

	t.Run("linked leaf", func(t *testing.T) {
		secretFile := filepath.Join(outside, "secret.txt")
		if err := os.WriteFile(secretFile, []byte(secret), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		// A perfectly valid id whose file points outside the directory.
		const id = "feedfacefeedface"
		link := filepath.Join(dir, id)
		if err := os.Symlink(secretFile, link); err != nil {
			t.Skipf("symlinks unavailable in this environment: %v", err)
		}
		defer func() { _ = os.Remove(link) }()

		res := (&readTool{}).Execute(context.Background(),
			CallInfo{SandboxRoots: []string{t.TempDir()}, TooloutDir: dir},
			map[string]any{"locator": TooloutScheme + id})

		if strings.Contains(res.Content, secret) {
			t.Errorf("followed a linked leaf out of the spill directory:\n%s", res.Content)
		}
		if res.Error == "" {
			t.Error("linked leaf was not refused")
		}
	})
}

// Paging must lose nothing and bound everything. A line-numbered continuation
// could do neither: a page ends mid-line, so the next line number skips that
// line's unseen remainder, and asking for the line itself can return the whole
// spill when the spill is one long line.
func TestRead_TooloutPagingIsLosslessAndBounded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "toolout")
	const id = "0123456789abcdef"

	// Deliberately hostile shape: one line far longer than a page, multi-byte
	// characters straddling the cuts, and no trailing newline.
	//
	// The rune is three bytes, not two. Two-byte runes sit on even offsets and
	// the page limit is even, so every cut would land on a boundary naturally
	// and the test would pass against raw byte slicing — proving nothing. At
	// three bytes per rune the limit falls inside a character and the cut has
	// to back up, which the offset assertion below checks actually happened.
	spill := strings.Repeat("€", tooloutPageLimit) + "\nshort line\n" +
		strings.Repeat("x", tooloutPageLimit*2) + "\ntail without newline"
	writeSpill(t, dir, id, spill)

	ci := CallInfo{SandboxRoots: []string{t.TempDir()}, TooloutDir: dir}

	var reassembled strings.Builder
	offset := 0
	backedUp := false
	for page := 0; ; page++ {
		if page > 100 {
			t.Fatal("paging did not terminate")
		}
		args := map[string]any{"locator": TooloutScheme + id}
		if offset > 0 {
			args["offset"] = offset
		}
		res := (&readTool{}).Execute(context.Background(), ci, args)
		if res.Error != "" {
			t.Fatalf("page at offset %d: %s", offset, res.Error)
		}
		if len(res.Content) > tooloutPageLimit*2 {
			t.Fatalf("page at offset %d returned %d bytes; the limit is %d",
				offset, len(res.Content), tooloutPageLimit)
		}
		if !utf8.ValidString(res.Content) {
			t.Fatalf("page at offset %d is not valid UTF-8", offset)
		}

		body, next, more := splitContinuation(t, res.Content)
		reassembled.WriteString(body)
		if !more {
			break
		}
		if next <= offset {
			t.Fatalf("continuation offset %d did not advance past %d", next, offset)
		}
		// A page shorter than the nominal limit means the cut backed up off a
		// character boundary, which is the behaviour under test. Without this
		// the assertions below pass against raw byte slicing.
		if next-offset < tooloutPageLimit {
			backedUp = true
		}
		offset = next
	}

	if !backedUp {
		t.Error("no cut ever backed up off a rune boundary; the test is not exercising rune-safe paging")
	}
	if got := reassembled.String(); got != spill {
		t.Errorf("reassembled %d bytes, want the spilled %d — paging is lossy",
			len(got), len(spill))
	}
}

// splitContinuation separates a page body from its trailing continuation note,
// returning the next offset and whether more output remains.
func splitContinuation(t *testing.T, content string) (body string, next int, more bool) {
	t.Helper()
	idx := strings.LastIndex(content, "\n… (bytes ")
	if idx < 0 {
		return content, 0, false // single unpaged response
	}
	note := content[idx:]
	body = content[:idx]
	if strings.Contains(note, "end of output") {
		return body, 0, false
	}
	var from, to, total, off int
	if _, err := fmt.Sscanf(note, "\n… (bytes %d-%d of %d; continue with locator toolout:%*s and offset %d",
		&from, &to, &total, &off); err != nil {
		// The locator verb consumes the trailing text; fall back to the last field.
		fields := strings.Fields(strings.TrimRight(note, ")"))
		parsed, convErr := strconv.Atoi(fields[len(fields)-1])
		if convErr != nil {
			t.Fatalf("cannot parse continuation note %q: %v / %v", note, err, convErr)
		}
		return body, parsed, true
	}
	return body, off, true
}

func TestRead_TooloutRejectsLineAddressing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "toolout")
	const id = "aaaabbbbccccdddd"
	writeSpill(t, dir, id, "alpha\nbravo\ncharlie\n")
	ci := CallInfo{SandboxRoots: []string{t.TempDir()}, TooloutDir: dir}

	// Including the shapes that previously fell through to an unranged first
	// page, so a mis-addressed read looked like it had succeeded.
	tests := []map[string]any{
		{"locator": TooloutScheme + id, "start_line": 0, "end_line": 3},
		{"locator": TooloutScheme + id, "start_line": -2, "end_line": -1},
		{"locator": TooloutScheme + id, "start_line": 2, "end_line": 3},
		{"locator": TooloutScheme + id, "end_line": 3},
	}
	for i, args := range tests {
		res := (&readTool{}).Execute(context.Background(), ci, args)
		if res.Error == "" {
			t.Errorf("case %d: line addressing accepted for a toolout handle, content %q", i, res.Content)
		}
	}
}

func TestRead_TooloutRejectsBadOffsets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "toolout")
	const id = "1111222233334444"
	writeSpill(t, dir, id, "small\n")
	ci := CallInfo{SandboxRoots: []string{t.TempDir()}, TooloutDir: dir}

	for _, offset := range []int{-1, 9999} {
		res := (&readTool{}).Execute(context.Background(), ci,
			map[string]any{"locator": TooloutScheme + id, "offset": offset})
		if res.Error == "" {
			t.Errorf("offset %d accepted, content %q", offset, res.Content)
		}
	}
	// The exact end is a valid resume point that yields nothing further.
	res := (&readTool{}).Execute(context.Background(), ci,
		map[string]any{"locator": TooloutScheme + id, "offset": len("small\n")})
	if res.Error != "" {
		t.Errorf("offset at end-of-file rejected: %s", res.Error)
	}
}

// Nothing validates a tool call's arguments against the schema between the
// model and Execute, so a malformed offset arrives as-is. Mapping it to zero
// turns a broken continuation into a valid one addressing the start of the
// file: a plausible page, with no sign the caller asked for something else.
func TestRead_TooloutRejectsMalformedOffsets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "toolout")
	const id = "9999888877776666"
	writeSpill(t, dir, id, strings.Repeat("payload\n", 100))
	ci := CallInfo{SandboxRoots: []string{t.TempDir()}, TooloutDir: dir}

	tests := []struct {
		name   string
		offset any
	}{
		{name: "string", offset: "16"},
		{name: "fractional", offset: 12.5},
		{name: "boolean", offset: true},
		{name: "array", offset: []any{1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := (&readTool{}).Execute(context.Background(), ci,
				map[string]any{"locator": TooloutScheme + id, "offset": tt.offset})
			if res.Error == "" {
				t.Errorf("offset %#v accepted, returned %d bytes", tt.offset, len(res.Content))
			}
		})
	}

	// An explicit null is absence, not a malformed value.
	res := (&readTool{}).Execute(context.Background(), ci,
		map[string]any{"locator": TooloutScheme + id, "offset": nil})
	if res.Error != "" {
		t.Errorf("null offset rejected: %s", res.Error)
	}
}

// An offset landing inside a character would put invalid UTF-8 at the head of
// the page. Only offsets this tool reported are on a boundary by construction.
func TestRead_TooloutRejectsMidRuneOffset(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "toolout")
	const id = "5555444433332222"
	writeSpill(t, dir, id, strings.Repeat("€", 50))
	ci := CallInfo{SandboxRoots: []string{t.TempDir()}, TooloutDir: dir}

	res := (&readTool{}).Execute(context.Background(), ci,
		map[string]any{"locator": TooloutScheme + id, "offset": 1})
	if res.Error == "" {
		t.Fatalf("mid-rune offset accepted, returned %q", res.Content)
	}
	if !utf8.ValidString(res.Content) {
		t.Error("rejection still emitted invalid UTF-8")
	}

	// A boundary offset is fine.
	if res := (&readTool{}).Execute(context.Background(), ci,
		map[string]any{"locator": TooloutScheme + id, "offset": 3}); res.Error != "" {
		t.Errorf("boundary offset rejected: %s", res.Error)
	}
}

// The spill root's identity is taken from an open handle, so a configured
// directory reached through a link still resolves to the same place its
// contents do. Comparing a handle-derived target against a spelling-derived
// root would reject every genuine spill in such a setup.
func TestRead_TooloutRootReachedThroughLink(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real-toolout")
	const id = "7777666655554444"
	writeSpill(t, realDir, id, "spilled through a linked root\n")

	linkedRoot := filepath.Join(base, "linked-toolout")
	mustLinkDir(t, realDir, linkedRoot)
	defer func() { _ = os.Remove(linkedRoot) }()

	ci := CallInfo{SandboxRoots: []string{t.TempDir()}, TooloutDir: linkedRoot}
	res := (&readTool{}).Execute(context.Background(), ci,
		map[string]any{"locator": TooloutScheme + id})

	if res.Error != "" {
		t.Fatalf("spill under a linked root was refused: %s", res.Error)
	}
	if !strings.Contains(res.Content, "spilled through a linked root") {
		t.Errorf("content = %q, want the spilled output", res.Content)
	}
}

// A missing spill directory is a different condition from a missing spill file,
// and saying so keeps the model from retrying a handle that was never the
// problem.
func TestRead_TooloutDirectoryMissing(t *testing.T) {
	ci := CallInfo{
		SandboxRoots: []string{t.TempDir()},
		TooloutDir:   filepath.Join(t.TempDir(), "never-created"),
	}
	res := (&readTool{}).Execute(context.Background(), ci,
		map[string]any{"locator": TooloutScheme + "abcdabcdabcdabcd"})

	if res.Error == "" {
		t.Fatal("expected an error for a missing spill directory")
	}
	if !strings.Contains(res.Error, "directory unavailable") {
		t.Errorf("error = %q, want it to name the directory as the problem", res.Error)
	}
}

// The spill root must be pinned before the target is opened. Resolving it
// afterwards lets a replacement of the directory in between redirect the policy
// root itself: the target opens outside, the root is then resolved to that same
// outside place, and containment compares two consistent values and passes.
//
// The hook stages exactly that replacement, in the one window where it matters.
func TestRead_TooloutRootPinnedBeforeTargetOpen(t *testing.T) {
	base := t.TempDir()
	const id = "3333222211110000"
	const secret = "SECRET-REACHED-BY-REPOINTING-THE-ROOT"

	realDir := filepath.Join(base, "real")
	writeSpill(t, realDir, id, "the genuine spill\n")
	evilDir := filepath.Join(base, "evil")
	writeSpill(t, evilDir, id, secret)

	root := filepath.Join(base, "root")
	mustLinkDir(t, realDir, root)
	defer func() { _ = os.Remove(root) }()

	swapped := false
	data, err := readTooloutFromRoot(root, TooloutScheme+id, func() {
		if swapped {
			return
		}
		swapped = true
		// Re-point the configured directory at the attacker's, after the root
		// has been pinned and before the target is opened.
		if err := os.Remove(root); err != nil {
			t.Fatalf("Remove root link: %v", err)
		}
		mustLinkDir(t, evilDir, root)
	})

	if !swapped {
		t.Fatal("the hook never ran; the ordering was not exercised")
	}
	// The directory handle still refers to the original directory, so the read
	// either serves the genuine spill or fails. What it must never do is follow
	// the name to the attacker's replacement.
	if strings.Contains(string(data), secret) {
		t.Errorf("a repointed root disclosed content from outside the pinned directory: %q", data)
	}
	if err == nil && !strings.Contains(string(data), "the genuine spill") {
		t.Errorf("the pinned read should serve the genuine spill, got %q", data)
	}
}

// The attack a pathname comparison cannot see: the replacement directory takes
// over the original name, so the target's canonical path still sits under the
// pinned string and containment agrees with itself.
//
// Renaming a directory while a handle on it is open is refused on Windows and
// permitted on Linux, so this runs on Linux and skips elsewhere. That is also
// why the junction cases cannot stand in for it — they change the canonical
// pathname, and this one deliberately does not.
func TestRead_TooloutSameNameDirectoryReplacement(t *testing.T) {
	base := t.TempDir()
	const id = "8888777766665555"
	const secret = "SECRET-VIA-SAME-NAME-REPLACEMENT"

	root := filepath.Join(base, "toolout")
	writeSpill(t, root, id, "the genuine spill\n")
	evil := filepath.Join(base, "evil")
	writeSpill(t, evil, id, secret)

	moved := filepath.Join(base, "moved-aside")
	swapped := false
	data, err := readTooloutFromRoot(root, TooloutScheme+id, func() {
		if swapped {
			return
		}
		swapped = true
		// Take the real directory out of the way and put the attacker's
		// directory under the exact name that was pinned.
		if err := os.Rename(root, moved); err != nil {
			t.Skipf("cannot rename a directory with an open handle here: %v", err)
		}
		if err := os.Rename(evil, root); err != nil {
			t.Fatalf("Rename evil into place: %v", err)
		}
	})

	if !swapped {
		t.Fatal("the hook never ran; the replacement was not staged")
	}
	if strings.Contains(string(data), secret) {
		t.Errorf("same-name replacement disclosed the attacker's file: %q", data)
	}
	if err != nil && strings.Contains(err.Error(), secret) {
		t.Errorf("secret leaked through the error path: %s", err.Error())
	}
}
