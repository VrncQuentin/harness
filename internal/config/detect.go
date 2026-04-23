package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
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
func Detect(binDir string) Suggestions {
	if binDir == "" {
		return Suggestions{}
	}
	return Suggestions{
		LlamaBinary: detectLlamaBinary(binDir),
		MainModel:   detectModels(binDir, false),
		EmbedModel:  detectModels(binDir, true),
	}
}

func detectLlamaBinary(binDir string) []string {
	exe := "llama-server"
	if runtime.GOOS == "windows" {
		exe = "llama-server.exe"
	}

	seeds := []string{
		filepath.Join(binDir, exe),
		filepath.Join(binDir, "llama.cpp", exe),
		filepath.Join(binDir, "llama.cpp", "bin", exe),
	}
	if runtime.GOOS == "windows" {
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			seeds = append(seeds,
				filepath.Join(pf, "llama.cpp", exe),
				filepath.Join(pf, "llama.cpp", "bin", exe),
			)
		}
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

func detectModels(binDir string, embed bool) []string {
	modelsDir := filepath.Join(binDir, "models")
	entries, err := os.ReadDir(modelsDir)
	if err != nil {
		return nil
	}
	var out []string
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
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if _, dup := seen[abs]; dup {
			continue
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	return out
}
