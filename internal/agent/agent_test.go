package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vrnc/harness/internal/memory"
)

// newRepoWithAgents creates a temp memory repo populated with the
// given files and returns a DirReader rooted at it.
func newRepoWithAgents(t *testing.T, files map[string]string) *memory.DirReader {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	return memory.NewDirReader(root)
}

// activeState is a trivial in-memory stand-in for the active-agent
// config column used by the DiskRegistry callbacks.
type activeState struct{ name string }

func (s *activeState) get() string           { return s.name }
func (s *activeState) set(n string) error    { s.name = n; return nil }
func (s *activeState) setErr(_ string) error { return errors.New("persist failed") }

func TestDiskRegistry_ListEmptyWhenAgentsMissing(t *testing.T) {
	mem := newRepoWithAgents(t, map[string]string{
		"global/rules.md": "x",
	})
	st := &activeState{}
	reg := NewDiskRegistry(mem, st.get, st.set)

	got, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List on repo without agents/ = %v, want empty", got)
	}
}

func TestDiskRegistry_ListFindsSubdirsSorted(t *testing.T) {
	mem := newRepoWithAgents(t, map[string]string{
		"agents/reviewer/persona.md": "r",
		"agents/coder/persona.md":    "c",
		"agents/zeta/notes.md":       "z", // subdir without persona still counts as an agent folder
	})
	st := &activeState{}
	reg := NewDiskRegistry(mem, st.get, st.set)

	got, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Agent{
		{Name: "coder", PersonaPath: "agents/coder/persona.md", RulesPath: "agents/coder/rules.md", NotesPath: "agents/coder/notes.md"},
		{Name: "reviewer", PersonaPath: "agents/reviewer/persona.md", RulesPath: "agents/reviewer/rules.md", NotesPath: "agents/reviewer/notes.md"},
		{Name: "zeta", PersonaPath: "agents/zeta/persona.md", RulesPath: "agents/zeta/rules.md", NotesPath: "agents/zeta/notes.md"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List =\n\t%v\nwant\n\t%v", got, want)
	}
}

func TestDiskRegistry_Get(t *testing.T) {
	mem := newRepoWithAgents(t, map[string]string{
		"agents/coder/persona.md": "c",
	})
	st := &activeState{}
	reg := NewDiskRegistry(mem, st.get, st.set)

	got, err := reg.Get("coder")
	if err != nil {
		t.Fatalf("Get coder: %v", err)
	}
	if got.Name != "coder" {
		t.Errorf("Get coder: name = %q, want %q", got.Name, "coder")
	}
	if got.PersonaPath != "agents/coder/persona.md" {
		t.Errorf("Get coder: PersonaPath = %q, want agents/coder/persona.md", got.PersonaPath)
	}
	if got.RulesPath != "agents/coder/rules.md" {
		t.Errorf("Get coder: RulesPath = %q, want agents/coder/rules.md", got.RulesPath)
	}
	if got.NotesPath != "agents/coder/notes.md" {
		t.Errorf("Get coder: NotesPath = %q, want agents/coder/notes.md", got.NotesPath)
	}
}

func TestDiskRegistry_GetUnknownWrapsErrNotExist(t *testing.T) {
	mem := newRepoWithAgents(t, map[string]string{
		"agents/coder/persona.md": "c",
	})
	st := &activeState{}
	reg := NewDiskRegistry(mem, st.get, st.set)

	_, err := reg.Get("reviewer")
	if err == nil {
		t.Fatal("Get reviewer: expected error, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Get reviewer: errors.Is(err, fs.ErrNotExist) = false, err = %v", err)
	}
}

func TestDiskRegistry_GetEmptyNameRejected(t *testing.T) {
	mem := newRepoWithAgents(t, nil)
	st := &activeState{}
	reg := NewDiskRegistry(mem, st.get, st.set)
	if _, err := reg.Get(""); err == nil {
		t.Fatal("Get empty: expected error, got nil")
	}
}

func TestDiskRegistry_Active(t *testing.T) {
	mem := newRepoWithAgents(t, nil)
	st := &activeState{name: "coder"}
	reg := NewDiskRegistry(mem, st.get, st.set)
	if got := reg.Active(); got != "coder" {
		t.Errorf("Active = %q, want coder", got)
	}
}

func TestDiskRegistry_SetActive(t *testing.T) {
	mem := newRepoWithAgents(t, map[string]string{
		"agents/coder/persona.md":    "c",
		"agents/reviewer/persona.md": "r",
	})
	st := &activeState{name: "coder"}
	reg := NewDiskRegistry(mem, st.get, st.set)

	if err := reg.SetActive("reviewer"); err != nil {
		t.Fatalf("SetActive reviewer: %v", err)
	}
	if st.name != "reviewer" {
		t.Errorf("active state = %q, want reviewer", st.name)
	}
}

func TestDiskRegistry_SetActiveEmptyClears(t *testing.T) {
	mem := newRepoWithAgents(t, map[string]string{
		"agents/coder/persona.md": "c",
	})
	st := &activeState{name: "coder"}
	reg := NewDiskRegistry(mem, st.get, st.set)

	if err := reg.SetActive(""); err != nil {
		t.Fatalf("SetActive empty: %v", err)
	}
	if st.name != "" {
		t.Errorf("active state = %q, want empty", st.name)
	}
}

func TestDiskRegistry_SetActiveUnknownRejected(t *testing.T) {
	mem := newRepoWithAgents(t, map[string]string{
		"agents/coder/persona.md": "c",
	})
	st := &activeState{name: "coder"}
	reg := NewDiskRegistry(mem, st.get, st.set)

	err := reg.SetActive("ghost")
	if err == nil {
		t.Fatal("SetActive ghost: expected error, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("SetActive ghost: errors.Is(err, fs.ErrNotExist) = false, err = %v", err)
	}
	if st.name != "coder" {
		t.Errorf("active state mutated to %q on failed SetActive", st.name)
	}
}

func TestDiskRegistry_SetActivePropagatesPersistError(t *testing.T) {
	mem := newRepoWithAgents(t, map[string]string{
		"agents/coder/persona.md": "c",
	})
	st := &activeState{name: ""}
	reg := NewDiskRegistry(mem, st.get, st.setErr)

	err := reg.SetActive("coder")
	if err == nil {
		t.Fatal("SetActive: expected error from setErr callback, got nil")
	}
}

func TestDiskRegistry_CreateMakesDirAndIsDiscoverable(t *testing.T) {
	mem := newRepoWithAgents(t, nil)
	st := &activeState{}
	reg := NewDiskRegistry(mem, st.get, st.set)

	got, err := reg.Create("coder")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := Agent{Name: "coder", PersonaPath: "agents/coder/persona.md", RulesPath: "agents/coder/rules.md", NotesPath: "agents/coder/notes.md"}
	if got != want {
		t.Errorf("Create =\n\t%v\nwant\n\t%v", got, want)
	}

	list, err := reg.List()
	if err != nil {
		t.Fatalf("List after Create: %v", err)
	}
	if !reflect.DeepEqual(list, []Agent{want}) {
		t.Errorf("List after Create = %v, want %v", list, []Agent{want})
	}
	if _, err := reg.Get("coder"); err != nil {
		t.Errorf("Get after Create: %v", err)
	}
}

func TestDiskRegistry_CreateRejectsDuplicate(t *testing.T) {
	mem := newRepoWithAgents(t, map[string]string{
		"agents/coder/persona.md": "c",
	})
	st := &activeState{}
	reg := NewDiskRegistry(mem, st.get, st.set)

	_, err := reg.Create("coder")
	if err == nil {
		t.Fatal("Create on existing agent: expected error, got nil")
	}
	if !errors.Is(err, ErrAgentExists) {
		t.Errorf("Create duplicate: errors.Is(err, ErrAgentExists) = false, err = %v", err)
	}
}

func TestDiskRegistry_CreateRejectsInvalidNames(t *testing.T) {
	mem := newRepoWithAgents(t, nil)
	st := &activeState{}
	reg := NewDiskRegistry(mem, st.get, st.set)

	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"slash", "foo/bar"},
		{"backslash", "foo\\bar"},
		{"space", "my agent"},
		{"leading dot", ".hidden"},
		{"leading dash", "-coder"},
		{"dot only", "."},
		{"dotdot", ".."},
		{"unicode", "réviseur"},
		{"too long", strings.Repeat("a", 65)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reg.Create(tc.in)
			if err == nil {
				t.Fatalf("Create(%q): expected error, got nil", tc.in)
			}
			if !errors.Is(err, ErrInvalidName) {
				t.Errorf("Create(%q): errors.Is(err, ErrInvalidName) = false, err = %v", tc.in, err)
			}
		})
	}
}

func TestDiskRegistry_CreateAcceptsAllowedNames(t *testing.T) {
	mem := newRepoWithAgents(t, nil)
	st := &activeState{}
	reg := NewDiskRegistry(mem, st.get, st.set)

	for _, n := range []string{"coder", "code-reviewer", "agent_2", "agent.v2", "Z9"} {
		t.Run(n, func(t *testing.T) {
			if _, err := reg.Create(n); err != nil {
				t.Fatalf("Create(%q): %v", n, err)
			}
		})
	}
}
