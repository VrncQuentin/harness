package memory

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

// Committer is the git commit surface used by layout-v2 path mapping.
type Committer interface {
	Commit(msg string, files []string) (string, error)
}

// LayoutV2Reader presents the logical memory paths over two physical
// layout-v2 repositories: the global project repo and the active project repo.
// It intentionally exposes only the global repo and the active project repo;
// non-active project paths are outside its contract and return an error.
type LayoutV2Reader struct {
	GlobalRoot string
	ActiveSlug string
	ActiveRoot string

	global *DirReader
	active *DirReader
}

var (
	_ Reader     = (*LayoutV2Reader)(nil)
	_ Repo       = (*LayoutV2Reader)(nil)
	_ DirLister  = (*LayoutV2Reader)(nil)
	_ DirCreator = (*LayoutV2Reader)(nil)
	_ FileWriter = (*LayoutV2Reader)(nil)
	_ DirRemover = (*LayoutV2Reader)(nil)
	_ Walker     = (*LayoutV2Reader)(nil)
)

// NewLayoutV2Reader returns a logical memory reader over globalRoot and
// activeRoot. Empty activeSlug resolves to global. Callers must provide a
// non-empty activeRoot when activeSlug is not global; the legacy no-root case
// aliases to global only so old tests fail closed rather than writing elsewhere.
func NewLayoutV2Reader(globalRoot, activeSlug, activeRoot string) *LayoutV2Reader {
	if activeSlug == "" {
		activeSlug = "global"
	}
	if activeRoot == "" || activeSlug == "global" {
		activeRoot = globalRoot
	}
	return &LayoutV2Reader{
		GlobalRoot: globalRoot,
		ActiveSlug: activeSlug,
		ActiveRoot: activeRoot,
		global:     NewDirReader(globalRoot),
		active:     NewDirReader(activeRoot),
	}
}

func (r *LayoutV2Reader) Read(relPath string) ([]byte, error) {
	m, err := r.mapPath(relPath)
	if err != nil {
		return nil, err
	}
	return m.reader.Read(m.physical)
}

func (r *LayoutV2Reader) Exists(relPath string) bool {
	m, err := r.mapPath(relPath)
	if err != nil {
		return false
	}
	return m.reader.Exists(m.physical)
}

func (r *LayoutV2Reader) Glob(pattern string) ([]string, error) {
	m, err := r.mapPath(pattern)
	if err != nil {
		return nil, err
	}
	matches, err := m.reader.Glob(m.physical)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, joinLogical(m.logicalPrefix, match))
	}
	sort.Strings(out)
	return out, nil
}

func (r *LayoutV2Reader) ListDirs(relPath string) ([]string, error) {
	if strings.TrimSpace(relPath) == "" {
		return r.listRootDirs(), nil
	}
	m, err := r.mapPath(relPath)
	if err != nil {
		return nil, err
	}
	return m.reader.ListDirs(m.physical)
}

func (r *LayoutV2Reader) MkdirAll(relPath string) error {
	m, err := r.mapPath(relPath)
	if err != nil {
		return err
	}
	return m.reader.MkdirAll(m.physical)
}

func (r *LayoutV2Reader) WriteFile(relPath string, data []byte) error {
	m, err := r.mapPath(relPath)
	if err != nil {
		return err
	}
	return m.reader.WriteFile(m.physical, data)
}

func (r *LayoutV2Reader) RemoveAll(relPath string) error {
	m, err := r.mapPath(relPath)
	if err != nil {
		return err
	}
	return m.reader.RemoveAll(m.physical)
}

func (r *LayoutV2Reader) Walk(relPath string) ([]Entry, error) {
	if strings.TrimSpace(relPath) == "" {
		return r.walkAll()
	}
	m, err := r.mapPath(relPath)
	if err != nil {
		return nil, err
	}
	entries, err := m.reader.Walk(m.physical)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		entry.Path = joinLogical(m.logicalPrefix, entry.Path)
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (r *LayoutV2Reader) listRootDirs() []string {
	return []string{"agents", "global", "projects"}
}

func (r *LayoutV2Reader) walkAll() ([]Entry, error) {
	entries, err := r.global.Walk(".")
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, Entry{Path: globalLogicalPath(entry.Path), Dir: entry.Dir, Size: entry.Size})
	}
	if r.ActiveSlug != "global" {
		entries, err = r.active.Walk(".")
		if err != nil {
			return nil, err
		}
		prefix := path.Join("projects", r.ActiveSlug)
		for _, entry := range entries {
			entry.Path = joinLogical(prefix, entry.Path)
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

type repoRole string

const (
	repoRoleGlobal repoRole = "global"
	repoRoleActive repoRole = "active"
)

type mappedPath struct {
	reader        *DirReader
	physical      string
	logicalPrefix string
	role          repoRole
}

func (r *LayoutV2Reader) mapPath(rel string) (mappedPath, error) {
	if strings.TrimSpace(rel) == "" {
		return mappedPath{}, fmt.Errorf("memory: empty path")
	}
	rel = path.Clean(strings.ReplaceAll(rel, "\\", "/"))
	if rel == "." {
		return mappedPath{}, fmt.Errorf("memory: ambiguous layout-v2 root path")
	}

	if rel == "global" || strings.HasPrefix(rel, "global/") {
		physical := strings.TrimPrefix(rel, "global/")
		if physical == "global" {
			physical = "."
		}
		return mappedPath{reader: r.global, physical: physical, logicalPrefix: "global", role: repoRoleGlobal}, nil
	}
	if rel == "agents" || strings.HasPrefix(rel, "agents/") {
		return mappedPath{reader: r.global, physical: rel, logicalPrefix: "", role: repoRoleGlobal}, nil
	}
	if rel == "projects/global" || strings.HasPrefix(rel, "projects/global/") {
		physical := strings.TrimPrefix(rel, "projects/global/")
		if physical == "projects/global" {
			physical = "."
		}
		return mappedPath{reader: r.global, physical: physical, logicalPrefix: "projects/global", role: repoRoleGlobal}, nil
	}

	activePrefix := path.Join("projects", r.ActiveSlug)
	if rel == activePrefix || strings.HasPrefix(rel, activePrefix+"/") {
		physical := strings.TrimPrefix(rel, activePrefix+"/")
		if physical == activePrefix {
			physical = "."
		}
		role := repoRoleActive
		reader := r.active
		if r.ActiveSlug == "global" {
			role = repoRoleGlobal
			reader = r.global
		}
		return mappedPath{reader: reader, physical: physical, logicalPrefix: activePrefix, role: role}, nil
	}

	return mappedPath{}, fmt.Errorf("memory: path %q is outside active layout-v2 repos", rel)
}

func globalLogicalPath(physical string) string {
	switch {
	case physical == "rules.md" || physical == "user.md" || physical == "facts.md":
		return path.Join("global", physical)
	case physical == "agents" || strings.HasPrefix(physical, "agents/"):
		return physical
	default:
		return path.Join("projects", "global", physical)
	}
}

func joinLogical(prefix, physical string) string {
	physical = path.Clean(physical)
	if physical == "." || physical == "" {
		return path.Clean(prefix)
	}
	if prefix == "" || prefix == "." {
		return physical
	}
	return path.Join(prefix, physical)
}

// LayoutV2Committer maps logical memory paths to the correct physical git
// repo and repo-relative paths before committing.
type LayoutV2Committer struct {
	Reader *LayoutV2Reader
	Global Committer
	Active Committer
}

func (c *LayoutV2Committer) Commit(msg string, files []string) (string, error) {
	if c == nil || c.Reader == nil {
		return "", errors.New("memory: layout-v2 committer is not configured")
	}
	if len(files) == 0 {
		return "", errors.New("memory: commit requires at least one file")
	}
	var role repoRole
	var repo Committer
	physical := make([]string, 0, len(files))
	for _, file := range files {
		m, err := c.Reader.mapPath(file)
		if err != nil {
			return "", err
		}
		if role == "" {
			role = m.role
			switch role {
			case repoRoleGlobal:
				repo = c.Global
			default:
				repo = c.Active
			}
		} else if role != m.role {
			return "", fmt.Errorf("memory: cannot commit files spanning layout-v2 repos")
		}
		physical = append(physical, m.physical)
	}
	if repo == nil {
		return "", fmt.Errorf("memory: no git repo configured for %s files", role)
	}
	return repo.Commit(msg, physical)
}
