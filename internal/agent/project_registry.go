package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"

	"github.com/vrnc/harness/internal/memory"
)

const (
	OriginGlobal        = "global"
	OriginExtendsGlobal = "extends-global"
	OriginProjectOnly   = "project-only"
	OriginProject       = "project"
)

// ProjectAgentFile records both the resolved content and the repo scope it came
// from. Persona and rules may fall back from the active project to the global
// repo; notes remain project-scoped when a project is active.
type ProjectAgentFile struct {
	Path    string
	Content string
	Origin  string
	Exists  bool
}

// ProjectAgent is the project-aware view of an agent used by UI surfaces that
// need to show and mutate the same scope the prompt assembler will read.
type ProjectAgent struct {
	Name    string
	Persona ProjectAgentFile
	Rules   ProjectAgentFile
	Notes   ProjectAgentFile
	Origin  string
}

// ProjectRegistry resolves agents across the global memory repo and the active
// project repo. It keeps filesystem path semantics inside the agent package so
// runtime/UI adapters do not need to guess whether a write should touch global
// files or project-local overrides.
type ProjectRegistry struct {
	Global      Registry
	GlobalMem   memory.Repo
	ActiveMem   memory.Repo
	ProjectSlug string
	GlobalSlug  string
	SetActiveFn func(string) error
}

func (r *ProjectRegistry) List() ([]ProjectAgent, error) {
	if r.Global == nil {
		return nil, errors.New("agent: global registry not configured")
	}
	names := map[string]struct{}{}
	globalAgents, err := r.Global.List()
	if err != nil {
		return nil, err
	}
	for _, a := range globalAgents {
		names[a.Name] = struct{}{}
	}
	if r.projectScoped() {
		projectNames, err := r.projectAgentNames()
		if err != nil {
			return nil, err
		}
		for _, name := range projectNames {
			names[name] = struct{}{}
		}
	}

	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	out := make([]ProjectAgent, 0, len(sorted))
	for _, name := range sorted {
		info, err := r.Get(name)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

func (r *ProjectRegistry) Get(name string) (ProjectAgent, error) {
	if name == "" {
		return ProjectAgent{}, fmt.Errorf("agent: name is empty")
	}
	if r.Global == nil {
		return ProjectAgent{}, errors.New("agent: global registry not configured")
	}
	if r.GlobalMem == nil {
		return ProjectAgent{}, errors.New("agent: global memory repo not configured")
	}
	if r.ActiveMem == nil {
		return ProjectAgent{}, errors.New("agent: active memory repo not configured")
	}

	globalAgent, globalErr := r.Global.Get(name)
	globalExists := globalErr == nil
	if globalErr != nil && !errors.Is(globalErr, fs.ErrNotExist) {
		return ProjectAgent{}, globalErr
	}

	projectExists := false
	if r.projectScoped() {
		var err error
		projectExists, err = r.projectAgentExists(name)
		if err != nil {
			return ProjectAgent{}, err
		}
	}
	if !globalExists && !projectExists {
		if globalErr != nil {
			return ProjectAgent{}, globalErr
		}
		return ProjectAgent{}, fmt.Errorf("agent: %q: %w", name, fs.ErrNotExist)
	}

	a := newAgent(name)
	if globalExists {
		a = globalAgent
	}
	info := ProjectAgent{Name: name, Origin: OriginGlobal}
	if r.projectScoped() && projectExists && globalExists {
		info.Origin = OriginExtendsGlobal
	} else if r.projectScoped() && projectExists {
		info.Origin = OriginProjectOnly
	}

	persona, err := r.resolveDefinitionFile(projectExists, a.PersonaPath, "persona.md")
	if err != nil {
		return ProjectAgent{}, err
	}
	info.Persona = persona
	rules, err := r.resolveDefinitionFile(projectExists, a.RulesPath, "rules.md")
	if err != nil {
		return ProjectAgent{}, err
	}
	info.Rules = rules
	notes, err := r.resolveNotesFile(projectExists, a.NotesPath)
	if err != nil {
		return ProjectAgent{}, err
	}
	info.Notes = notes
	return info, nil
}

func (r *ProjectRegistry) Active() string {
	if r.Global == nil {
		return ""
	}
	return r.Global.Active()
}

func (r *ProjectRegistry) SetActive(name string) error {
	if name != "" {
		if _, err := r.Get(name); err != nil {
			return err
		}
	}
	if r.projectScoped() && r.SetActiveFn != nil {
		if err := r.SetActiveFn(name); err != nil {
			return fmt.Errorf("agent: persist active %q: %w", name, err)
		}
		return nil
	}
	if r.Global == nil {
		return errors.New("agent: global registry not configured")
	}
	return r.Global.SetActive(name)
}

func (r *ProjectRegistry) Create(name string) (ProjectAgent, error) {
	if err := ValidateName(name); err != nil {
		return ProjectAgent{}, err
	}
	if r.projectScoped() {
		if _, err := r.Get(name); err == nil {
			return ProjectAgent{}, fmt.Errorf("agent: %q: %w", name, ErrAgentExists)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return ProjectAgent{}, err
		}
		if err := r.ActiveMem.MkdirAll(agentDir(name)); err != nil {
			return ProjectAgent{}, fmt.Errorf("agent: create project agent %q: %w", name, err)
		}
		return r.Get(name)
	}
	if r.Global == nil {
		return ProjectAgent{}, errors.New("agent: global registry not configured")
	}
	if _, err := r.Global.Create(name); err != nil {
		return ProjectAgent{}, err
	}
	return r.Get(name)
}

func (r *ProjectRegistry) WritePersona(name string, body []byte) error {
	return r.writeScopedFile(name, "persona.md", body, func() error {
		return r.Global.WritePersona(name, body)
	})
}

func (r *ProjectRegistry) WriteRules(name string, body []byte) error {
	return r.writeScopedFile(name, "rules.md", body, func() error {
		return r.Global.WriteRules(name, body)
	})
}

func (r *ProjectRegistry) WriteNotes(name string, body []byte) error {
	return r.writeScopedFile(name, "notes.md", body, func() error {
		return r.Global.WriteNotes(name, body)
	})
}

func (r *ProjectRegistry) Delete(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if r.projectScoped() {
		exists, err := r.projectAgentExists(name)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("agent: project override %q: %w", name, fs.ErrNotExist)
		}
		if r.Active() == name {
			if err := r.SetActive(""); err != nil {
				return fmt.Errorf("agent: delete project agent %q: clear active: %w", name, err)
			}
		}
		if err := r.ActiveMem.RemoveAll(agentDir(name)); err != nil {
			return fmt.Errorf("agent: delete project agent %q: %w", name, err)
		}
		return nil
	}
	if r.Global == nil {
		return errors.New("agent: global registry not configured")
	}
	return r.Global.Delete(name)
}

func (r *ProjectRegistry) writeScopedFile(name, file string, body []byte, writeGlobal func() error) error {
	if _, err := r.Get(name); err != nil {
		return err
	}
	if r.projectScoped() {
		rel := path.Join(agentDir(name), file)
		if err := r.ActiveMem.WriteFile(rel, body); err != nil {
			return fmt.Errorf("agent: write project %s %q: %w", file, name, err)
		}
		return nil
	}
	return writeGlobal()
}

func (r *ProjectRegistry) resolveDefinitionFile(projectExists bool, relPath, file string) (ProjectAgentFile, error) {
	if r.projectScoped() && projectExists {
		projectPath := path.Join(path.Dir(relPath), file)
		f, err := readProjectFile(r.ActiveMem, projectPath, OriginProject)
		if err != nil {
			return ProjectAgentFile{}, err
		}
		if f.Exists {
			return f, nil
		}
	}
	return readProjectFile(r.GlobalMem, relPath, OriginGlobal)
}

func (r *ProjectRegistry) resolveNotesFile(projectExists bool, relPath string) (ProjectAgentFile, error) {
	if r.projectScoped() {
		f, err := readProjectFile(r.ActiveMem, relPath, OriginProject)
		if err != nil {
			return ProjectAgentFile{}, err
		}
		if projectExists || f.Exists {
			return f, nil
		}
		return ProjectAgentFile{Path: relPath, Origin: OriginProject}, nil
	}
	return readProjectFile(r.GlobalMem, relPath, OriginGlobal)
}

func readProjectFile(mem memory.Reader, relPath, origin string) (ProjectAgentFile, error) {
	f := ProjectAgentFile{Path: relPath, Origin: origin}
	b, err := mem.Read(relPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return f, nil
		}
		return f, err
	}
	f.Content = string(b)
	f.Exists = true
	return f, nil
}

func (r *ProjectRegistry) projectAgentExists(name string) (bool, error) {
	names, err := r.projectAgentNames()
	if err != nil {
		return false, err
	}
	return slices.Contains(names, name), nil
}

func (r *ProjectRegistry) projectAgentNames() ([]string, error) {
	if r.ActiveMem == nil {
		return nil, errors.New("agent: active memory repo not configured")
	}
	names, err := r.ActiveMem.ListDirs(agentsDir)
	if err != nil {
		return nil, fmt.Errorf("agent: list project agents: %w", err)
	}
	return names, nil
}

func (r *ProjectRegistry) projectScoped() bool {
	if r.ProjectSlug == "" {
		return false
	}
	globalSlug := r.GlobalSlug
	if globalSlug == "" {
		globalSlug = "global"
	}
	return r.ProjectSlug != globalSlug
}

func agentDir(name string) string {
	return path.Join(agentsDir, name)
}
