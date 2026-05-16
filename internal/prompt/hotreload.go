package prompt

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// hotReloadDebounce collapses rapid save bursts (atomic writes emit a
// flurry of events on Windows) into a single reload notification.
const hotReloadDebounce = 200 * time.Millisecond

// HotReload watches the files that feed the prompt assembler and
// emits a log entry every time one of them changes. M2 doesn't yet
// have consumers that react to the change (the DiskAssembler re-reads
// from disk on every Assemble call), but logging here gives the
// operator the visibility the roadmap's "edit on disk and next
// request reflects the change" acceptance test calls for.
type HotReload struct {
	repoPath string
	logger   *slog.Logger

	mu                sync.Mutex
	watcher           *fsnotify.Watcher
	activeAgent       string
	activeProjectSlug string
	watched           map[string]struct{}

	// debounceMu guards the per-path timers. Separate from mu so the
	// event pump doesn't hold the big lock while scheduling timers.
	debounceMu sync.Mutex
	debounce   map[string]*time.Timer

	closed chan struct{}
	done   chan struct{}
}

// NewHotReload builds a HotReload watching the global files, the
// active project rules, and the files for activeAgent (may be empty).
// Missing files are logged as warnings; construction only fails if
// fsnotify itself cannot be initialised.
func NewHotReload(repoPath string, agentName string, projectSlug string, logger *slog.Logger) (*HotReload, error) {
	if logger == nil {
		logger = slog.Default()
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("prompt: hotreload watcher: %w", err)
	}
	h := &HotReload{
		repoPath:          repoPath,
		logger:            logger,
		watcher:           w,
		activeAgent:       agentName,
		activeProjectSlug: projectSlug,
		watched:           make(map[string]struct{}),
		debounce:          make(map[string]*time.Timer),
		closed:            make(chan struct{}),
		done:              make(chan struct{}),
	}
	h.refreshWatches()
	go h.run()
	return h, nil
}

// SetActiveAgent rewires the persona/notes watches when the operator
// switches agents. The global and project files remain watched.
func (h *HotReload) SetActiveAgent(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.activeAgent == name {
		return
	}
	h.activeAgent = name
	h.refreshWatches()
}

// SetActiveProject rewires the project rules watch when the operator
// switches projects. The global and agent files remain watched.
func (h *HotReload) SetActiveProject(slug string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.activeProjectSlug == slug {
		return
	}
	h.activeProjectSlug = slug
	h.refreshWatches()
}

// Close stops the watcher and waits for the event loop to drain.
// Idempotent; a second call returns the first Close's error.
func (h *HotReload) Close() error {
	h.mu.Lock()
	select {
	case <-h.closed:
		h.mu.Unlock()
		<-h.done
		return nil
	default:
		close(h.closed)
	}
	w := h.watcher
	h.mu.Unlock()

	err := w.Close()
	<-h.done

	// Stop any pending debounced timers so tests don't leak goroutines.
	h.debounceMu.Lock()
	for _, t := range h.debounce {
		t.Stop()
	}
	h.debounce = nil
	h.debounceMu.Unlock()

	if err != nil {
		return fmt.Errorf("prompt: hotreload close: %w", err)
	}
	return nil
}

// refreshWatches synchronises fsnotify's watch list with the files we
// care about. Caller must hold h.mu.
//
// Why watch parent directories instead of individual files: editors
// frequently save via atomic rename (write a temp file, rename into
// place) which fsnotify reports as a REMOVE followed by CREATE on the
// watched path - the file handle we registered is now stale. Watching
// the parent directory catches both paths of any atomic save.
func (h *HotReload) refreshWatches() {
	wanted := h.wantedDirs()

	for dir := range h.watched {
		if _, ok := wanted[dir]; !ok {
			if err := h.watcher.Remove(dir); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
				h.logger.Warn("prompt: hotreload remove watch", "dir", dir, "err", err)
			}
			delete(h.watched, dir)
		}
	}

	for dir := range wanted {
		if _, ok := h.watched[dir]; ok {
			continue
		}
		if _, err := os.Stat(dir); err != nil {
			if !os.IsNotExist(err) {
				h.logger.Warn("prompt: hotreload stat", "dir", dir, "err", err)
			}
			// Missing directory is not fatal - the operator may not
			// have created the agent folder yet. Log once at debug
			// level so it's visible during triage.
			h.logger.Debug("prompt: hotreload skipping missing dir", "dir", dir)
			continue
		}
		if err := h.watcher.Add(dir); err != nil {
			h.logger.Warn("prompt: hotreload add watch", "dir", dir, "err", err)
			continue
		}
		h.watched[dir] = struct{}{}
	}
}

// wantedDirs returns the set of directories we need to watch given
// the current active agent and project. Global files share global/,
// project files share projects/<slug>/, and per-agent files share
// agents/<name>/.
func (h *HotReload) wantedDirs() map[string]struct{} {
	out := map[string]struct{}{
		filepath.Join(h.repoPath, "global"): {},
	}
	slug := h.activeProjectSlug
	if slug == "" {
		slug = "global"
	}
	out[filepath.Join(h.repoPath, "projects", slug)] = struct{}{}
	if h.activeAgent != "" {
		out[filepath.Join(h.repoPath, "agents", h.activeAgent)] = struct{}{}
	}
	return out
}

// watchedFiles returns the set of concrete files we emit events for.
// Changes to other files inside a watched directory are ignored to
// keep the signal clean.
func (h *HotReload) watchedFiles() map[string]struct{} {
	out := map[string]struct{}{
		filepath.Join(h.repoPath, "global", "rules.md"): {},
		filepath.Join(h.repoPath, "global", "user.md"):  {},
		filepath.Join(h.repoPath, "global", "facts.md"): {},
	}
	h.mu.Lock()
	slug := h.activeProjectSlug
	ag := h.activeAgent
	h.mu.Unlock()
	if slug == "" {
		slug = "global"
	}
	out[filepath.Join(h.repoPath, "projects", slug, "rules.md")] = struct{}{}
	if ag != "" {
		base := filepath.Join(h.repoPath, "agents", ag)
		out[filepath.Join(base, "persona.md")] = struct{}{}
		out[filepath.Join(base, "rules.md")] = struct{}{}
		out[filepath.Join(base, "notes.md")] = struct{}{}
	}
	return out
}

// run is the fsnotify event pump. It filters events down to the
// watched file set, debounces bursts, and logs one entry per path per
// debounce window.
func (h *HotReload) run() {
	defer close(h.done)
	for {
		select {
		case <-h.closed:
			return
		case err, ok := <-h.watcher.Errors:
			if !ok {
				return
			}
			if err != nil {
				h.logger.Warn("prompt: hotreload watch error", "err", err)
			}
		case ev, ok := <-h.watcher.Events:
			if !ok {
				return
			}
			if !h.shouldFire(ev) {
				continue
			}
			h.schedule(ev.Name)
		}
	}
}

// shouldFire decides whether ev targets a file we care about. We only
// fire on writes, creates, and renames - chmod and remove-with-no-
// replacement produce noise.
func (h *HotReload) shouldFire(ev fsnotify.Event) bool {
	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
		return false
	}
	_, ok := h.watchedFiles()[ev.Name]
	return ok
}

// schedule arms (or resets) a debounce timer for path so the eventual
// log line fires exactly once per burst.
func (h *HotReload) schedule(path string) {
	h.debounceMu.Lock()
	defer h.debounceMu.Unlock()
	if h.debounce == nil {
		return
	}
	if t, ok := h.debounce[path]; ok {
		t.Reset(hotReloadDebounce)
		return
	}
	h.debounce[path] = time.AfterFunc(hotReloadDebounce, func() {
		h.debounceMu.Lock()
		// delete on a nil map is a no-op, so a race with Close is
		// harmless here.
		delete(h.debounce, path)
		h.debounceMu.Unlock()
		h.logger.Info("prompt: file changed", "path", path)
	})
}
