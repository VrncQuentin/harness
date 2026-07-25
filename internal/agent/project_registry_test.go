package agent

import (
	"errors"
	"io/fs"
	"reflect"
	"testing"

	"github.com/VrncQuentin/harness/internal/memory"
)

func newProjectRegistryForTest(t *testing.T, globalFiles, projectFiles map[string]string) (*ProjectRegistry, *memoryReposForProjectRegistryTest) {
	t.Helper()
	globalMem := newRepoWithAgents(t, globalFiles)
	projectMem := newRepoWithAgents(t, projectFiles)
	active := &activeState{}
	globalReg := NewDiskRegistry(globalMem, active.get, active.set)
	repos := &memoryReposForProjectRegistryTest{
		global: globalMem,
		active: projectMem,
		state:  active,
	}
	return &ProjectRegistry{
		Global:      globalReg,
		GlobalMem:   globalMem,
		ActiveMem:   projectMem,
		ProjectSlug: "dt",
		GlobalSlug:  "global",
		SetActiveFn: active.set,
	}, repos
}

type memoryReposForProjectRegistryTest struct {
	global *memory.DirReader
	active *memory.DirReader
	state  *activeState
}

func TestProjectRegistryListMergesGlobalAndProjectAgents(t *testing.T) {
	reg, _ := newProjectRegistryForTest(t,
		map[string]string{
			"agents/coder/persona.md":    "global coder",
			"agents/reviewer/persona.md": "global reviewer",
		},
		map[string]string{
			"agents/coder/notes.md":   "project notes",
			"agents/local/persona.md": "project local",
		},
	)

	got, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	compact := make([]ProjectAgent, 0, len(got))
	for _, a := range got {
		compact = append(compact, ProjectAgent{Name: a.Name, Origin: a.Origin})
	}
	want := []ProjectAgent{
		{Name: "coder", Origin: OriginExtendsGlobal},
		{Name: "local", Origin: OriginProjectOnly},
		{Name: "reviewer", Origin: OriginGlobal},
	}
	if !reflect.DeepEqual(compact, want) {
		t.Fatalf("List names/origins = %#v, want %#v", compact, want)
	}
}

func TestProjectRegistryGetUsesProjectDefinitionOverridesAndGlobalFallback(t *testing.T) {
	reg, _ := newProjectRegistryForTest(t,
		map[string]string{
			"agents/coder/persona.md": "global persona",
			"agents/coder/rules.md":   "global rules",
			"agents/coder/notes.md":   "global notes",
		},
		map[string]string{
			"agents/coder/persona.md": "project persona",
			"agents/coder/notes.md":   "project notes",
		},
	)

	got, err := reg.Get("coder")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Origin != OriginExtendsGlobal {
		t.Fatalf("Origin = %q, want %q", got.Origin, OriginExtendsGlobal)
	}
	if got.Persona.Content != "project persona" || got.Persona.Origin != OriginProject {
		t.Fatalf("Persona = %+v, want project override", got.Persona)
	}
	if got.Rules.Content != "global rules" || got.Rules.Origin != OriginGlobal {
		t.Fatalf("Rules = %+v, want global fallback", got.Rules)
	}
	if got.Notes.Content != "project notes" || got.Notes.Origin != OriginProject {
		t.Fatalf("Notes = %+v, want project notes", got.Notes)
	}
}

func TestProjectRegistrySetActiveAcceptsProjectOnlyAgent(t *testing.T) {
	reg, repos := newProjectRegistryForTest(t,
		map[string]string{},
		map[string]string{"agents/local/persona.md": "project local"},
	)

	if err := reg.SetActive("local"); err != nil {
		t.Fatalf("SetActive project-only agent: %v", err)
	}
	if repos.state.name != "local" {
		t.Fatalf("active = %q, want local", repos.state.name)
	}
}

func TestProjectRegistryWritesProjectScopedOverrides(t *testing.T) {
	reg, repos := newProjectRegistryForTest(t,
		map[string]string{
			"agents/coder/persona.md": "global persona",
			"agents/coder/rules.md":   "global rules",
			"agents/coder/notes.md":   "global notes",
		},
		map[string]string{"agents/coder/notes.md": "old project notes"},
	)

	if err := reg.WritePersona("coder", []byte("project persona")); err != nil {
		t.Fatalf("WritePersona: %v", err)
	}
	if err := reg.WriteRules("coder", []byte("project rules")); err != nil {
		t.Fatalf("WriteRules: %v", err)
	}
	if err := reg.WriteNotes("coder", []byte("project notes")); err != nil {
		t.Fatalf("WriteNotes: %v", err)
	}

	for rel, want := range map[string]string{
		"agents/coder/persona.md": "project persona",
		"agents/coder/rules.md":   "project rules",
		"agents/coder/notes.md":   "project notes",
	} {
		got, err := repos.active.Read(rel)
		if err != nil {
			t.Fatalf("read project %s: %v", rel, err)
		}
		if string(got) != want {
			t.Fatalf("project %s = %q, want %q", rel, string(got), want)
		}
	}
	for rel, want := range map[string]string{
		"agents/coder/persona.md": "global persona",
		"agents/coder/rules.md":   "global rules",
		"agents/coder/notes.md":   "global notes",
	} {
		got, err := repos.global.Read(rel)
		if err != nil {
			t.Fatalf("read global %s: %v", rel, err)
		}
		if string(got) != want {
			t.Fatalf("global %s changed to %q, want %q", rel, string(got), want)
		}
	}
}

func TestProjectRegistryDeleteRemovesOnlyProjectScopeAndClearsActive(t *testing.T) {
	reg, repos := newProjectRegistryForTest(t,
		map[string]string{
			"agents/coder/persona.md": "global persona",
			"agents/coder/rules.md":   "global rules",
		},
		map[string]string{
			"agents/coder/persona.md": "project persona",
			"agents/coder/notes.md":   "project notes",
		},
	)
	repos.state.name = "coder"

	if err := reg.Delete("coder"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if repos.state.name != "" {
		t.Fatalf("active = %q, want cleared", repos.state.name)
	}
	if _, err := repos.active.Read("agents/coder/persona.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("project persona after Delete err = %v, want fs.ErrNotExist", err)
	}
	got, err := repos.global.Read("agents/coder/persona.md")
	if err != nil {
		t.Fatalf("global persona after Delete: %v", err)
	}
	if string(got) != "global persona" {
		t.Fatalf("global persona = %q, want unchanged", string(got))
	}
}
