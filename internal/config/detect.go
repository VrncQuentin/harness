package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/VrncQuentin/harness/internal/pathid"
)

// Suggestions holds candidate values the /config form can offer on first run so
// the user does not have to type full paths by hand. All slices hold absolute
// paths that exist on disk at the time Detect was called. An empty slice means
// "nothing found, leave the field blank".
type Suggestions struct {
	LlamaBinary []string
	MainModel   []string
	EmbedModel  []string
}

// Detect scans well-known locations relative to binDir (and $PATH, where
// relevant) for llama-server and .gguf model files. It never returns an error:
// missing or unreadable paths are simply absent from the result.
//
// binDir is typically the directory containing the running harness binary. If
// empty, Detect returns zero-value Suggestions so tests and callers without a
// resolved binary path get deterministic output.
//
// For dev ergonomics Detect also searches binDir's parent - the common layout
// is dist/harness.exe alongside a sibling models/ and llama.cpp/ at the repo
// root, so binDir alone misses both.
//
// The scans below stat and list by pathname, and deliberately stay that way.
// This is not an authorization boundary: it produces *suggestions* for a form
// the user then edits and saves, and every value it offers is validated again
// on save and resolved again when the process is actually spawned. The
// locations searched are also not a configured tree the harness owns — they are
// wherever the operator happens to keep llama.cpp and their models — so there
// is no root to resolve them through. See the filesystem access ledger in
// docs/architecture.md.
func Detect(binDir string) Suggestions {
	if binDir == "" {
		return Suggestions{}
	}
	roots := searchRoots(binDir)
	return Suggestions{
		LlamaBinary: detectLlamaBinary(roots),
		MainModel:   detectModels(roots, false /* embed */),
		EmbedModel:  detectModels(roots, true /* embed */),
	}
}

// searchRoots returns binDir plus its parent (when distinct). Parent covers
// the dev layout where the built binary sits in dist/ while resources live at
// the repo root.
func searchRoots(binDir string) []string {
	roots := []string{binDir}
	if parent := filepath.Dir(binDir); parent != "" && parent != binDir {
		roots = append(roots, parent)
	}
	return roots
}

func detectLlamaBinary(roots []string) []string {
	exe := "llama-server"
	if runtime.GOOS == "windows" {
		exe = "llama-server.exe"
	}

	var seeds []string
	for _, r := range roots {
		seeds = append(seeds,
			filepath.Join(r, exe),
			filepath.Join(r, "llama.cpp", exe),
			filepath.Join(r, "llama.cpp", "bin", exe),
		)
	}
	if runtime.GOOS == "windows" {
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			seeds = append(seeds,
				filepath.Join(pf, "llama.cpp", exe),
				filepath.Join(pf, "llama.cpp", "bin", exe),
			)
		}
	} else {
		if home, _ := os.UserHomeDir(); home != "" {
			for _, d := range []string{"bin", ".local/bin", "llama.cpp/bin"} {
				seeds = append(seeds, filepath.Join(home, d, exe))
			}
		}
		seeds = append(seeds, filepath.Join("/opt/llama.cpp/bin", exe))
	}

	var found []string
	if p, err := exec.LookPath(exe); err == nil {
		found = append(found, p)
	}
	for _, s := range seeds {
		fi, err := os.Stat(s)
		if err != nil || fi.IsDir() {
			continue
		}
		found = append(found, s)
	}
	return dedupAbs(found)
}

func detectModels(roots []string, embed bool) []string {
	var out []string
	for _, r := range roots {
		modelsDir := filepath.Join(r, "models")
		entries, err := os.ReadDir(modelsDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.EqualFold(filepath.Ext(name), ".gguf") {
				continue
			}
			if looksLikeEmbedder(name) != embed {
				continue
			}
			if abs, err := filepath.Abs(filepath.Join(modelsDir, name)); err == nil {
				out = append(out, abs)
			}
		}
	}
	out = dedupAbs(out)
	sort.Strings(out)
	return out
}

func looksLikeEmbedder(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "embed") || strings.Contains(n, "nomic")
}

func dedupAbs(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		abs, key := canonicalPath(p)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, abs)
	}
	return out
}

// canonicalPath returns the path to display for a suggestion and the key that
// decides whether it duplicates one already collected.
//
// The key is the physical identity from internal/pathid, so two spellings of
// one binary — a symlink on $PATH, a junction, an 8.3 alias, a different case
// on Windows — collapse into a single suggestion instead of offering the user
// the same file twice. filepath.EvalSymlinks, which this replaced, leaves a
// junction unresolved and is case-sensitive, so neither collapsed.
//
// Detection is best-effort by contract: Detect returns no error, and a path it
// cannot resolve is still a real candidate the user may want. So resolution
// failure falls back to the lexical absolute path as the key. That fallback
// only ever costs a duplicate row in a suggestion list; nothing here is a
// boundary, which is why it is acceptable here and nowhere else in this
// repository.
func canonicalPath(p string) (display string, key string) {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	id, err := pathid.Resolve(abs)
	if err != nil {
		return abs, abs
	}
	return abs, id.Key()
}
