package ui

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/vrnc/harness/internal/logbuf"
	"github.com/vrnc/harness/internal/memory"
)

// statusLogTail is the number of recent log entries shown server-rendered on
// the status page. New entries are appended client-side via SSE.
const statusLogTail = 100

// procLogTail is the number of recent output lines shown server-rendered for
// each proc's log card. New entries are appended client-side via SSE, so this
// only controls the initial seed on page load.
const procLogTail = 50

// statusPageData is the template context for the status page.
type statusPageData struct {
	basePage
	stateSnapshot
	QueuePct        int
	HasRetry        bool
	StartupErrText  []string
	MemoryLayout    memoryLayoutView
	ScaffoldErr     string
	ScaffoldCreated int
	LlamaPanel      procStatusPanelData
	EmbedPanel      procStatusPanelData
	HarnessLog      logboxData
	LlamaLog        logboxData
	EmbedLog        logboxData
}

type queueCardData struct {
	QueueDepth int
	QueueMax   int
	QueuePct   int
}

type procStatusPanelData struct {
	PanelID string
	ProcID  string
	Title   string
	Status  ProcessStatus
}

// memoryLayoutView is the template-friendly form of the missing-items
// list. Show is the gating boolean - the template uses it instead of
// `{{if .MemoryLayout.Items}}` so a non-error "no missing items" outcome
// reads as a single explicit flag.
type memoryLayoutView struct {
	Show     bool
	RepoPath string
	Items    []memoryLayoutItemView
}

// memoryLayoutItemView mirrors memory.LayoutItem with a label the
// template can render directly without calling a helper.
type memoryLayoutItemView struct {
	Path string
	Kind string
	Desc string
}

// logboxData is the data passed to the shared "logbox" template partial. One
// instance per card: harness logs, llama-server output, embedder output.
type logboxData struct {
	BodyID    string
	EventName string
	Entries   []logEntryView
}

// logEntryView is the template-friendly form of a logbuf entry.
type logEntryView struct {
	Time string
	Line string
}

// handleStatus renders the status page. Only the root path renders the page;
// any other unknown path returns 404 so we don't shadow future routes.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	snap := s.state.snapshot()

	errTexts := make([]string, 0, len(snap.StartupErrors))
	for _, e := range snap.StartupErrors {
		errTexts = append(errTexts, e.Error())
	}

	scaffoldCreated := 0
	if v := r.URL.Query().Get("scaffold_created"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			scaffoldCreated = n
		}
	}

	data := statusPageData{
		basePage:        s.newBasePage("status"),
		stateSnapshot:   snap,
		QueuePct:        queuePct(snap.QueueDepth, snap.QueueMax),
		HasRetry:        s.hasRetry(),
		StartupErrText:  errTexts,
		MemoryLayout:    s.memoryLayoutView(),
		ScaffoldErr:     r.URL.Query().Get("scaffold_err"),
		ScaffoldCreated: scaffoldCreated,
		LlamaPanel:      llamaPanelFromSnapshot(snap),
		EmbedPanel:      embedPanelFromSnapshot(snap),
		HarnessLog:      logboxData{BodyID: "harness-log", EventName: "harness-log", Entries: recentEntries(s.getLogRing(), statusLogTail)},
		LlamaLog:        logboxData{BodyID: "llama-log", EventName: "llama-log", Entries: recentEntries(s.getLlamaRing(), procLogTail)},
		EmbedLog:        logboxData{BodyID: "embed-log", EventName: "embed-log", Entries: recentEntries(s.getEmbedRing(), procLogTail)},
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.statusTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// memoryLayoutView computes the template view of the missing canonical
// items in the configured memory repo. The prompt is only shown when:
//   - a memory repo path is configured,
//   - the path resolves to an existing directory, AND
//   - one or more canonical items are missing.
//
// Errors stating the path is missing/unreadable are deliberately
// swallowed: those conditions are surfaced separately as startup errors
// (M3) or via the agents page setup CTA. We do not want two different
// alerts pointing at the same root cause on the status page.
func (s *Server) memoryLayoutView() memoryLayoutView {
	path := s.getMemoryRepoPath()
	if path == "" {
		return memoryLayoutView{}
	}
	missing, err := memory.MissingItems(path)
	if err != nil || len(missing) == 0 {
		return memoryLayoutView{}
	}
	items := make([]memoryLayoutItemView, 0, len(missing))
	for _, m := range missing {
		kind := "file"
		if m.Dir {
			kind = "directory"
		}
		items = append(items, memoryLayoutItemView{Path: m.Path, Kind: kind, Desc: m.Desc})
	}
	return memoryLayoutView{Show: true, RepoPath: path, Items: items}
}

func queuePct(depth, capacity int) int {
	if capacity <= 0 {
		return 0
	}
	p := depth * 100 / capacity
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

func queueCardFromSnapshot(s stateSnapshot) queueCardData {
	return queueCardData{
		QueueDepth: s.QueueDepth,
		QueueMax:   s.QueueMax,
		QueuePct:   queuePct(s.QueueDepth, s.QueueMax),
	}
}

func llamaPanelFromSnapshot(s stateSnapshot) procStatusPanelData {
	return procStatusPanelData{PanelID: "llama-status-panel", ProcID: "llama", Title: "llama-server", Status: s.LlamaStatus}
}

func embedPanelFromSnapshot(s stateSnapshot) procStatusPanelData {
	return procStatusPanelData{PanelID: "embed-status-panel", ProcID: "embed", Title: "Embedder", Status: s.EmbedderStatus}
}

// recentEntries returns the last n log entries from ring formatted for the
// status template. Returns nil if the ring is not wired up.
func recentEntries(ring *logbuf.Ring, n int) []logEntryView {
	if ring == nil {
		return nil
	}
	all := ring.Snapshot()
	if len(all) > n {
		all = all[len(all)-n:]
	}
	out := make([]logEntryView, len(all))
	for i, e := range all {
		out[i] = logEntryView{
			Time: e.Time.Format("15:04:05"),
			Line: e.Line,
		}
	}
	return out
}

// handleRetry is POST /retry - clears startup errors and re-runs validation.
func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.callRetry()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleMemoryScaffold is POST /memory/scaffold - creates each missing
// canonical item under the configured memory.repo_path. The redirect
// always lands back on the status page; a non-empty scaffold_err query
// param causes the prompt to render an error banner above the (now
// possibly shorter) missing-items list.
//
// The handler refuses to act if no path is configured or the path no
// longer points at a directory; in either case the user is redirected
// with an explanatory message so the action is not silently swallowed.
func (s *Server) handleMemoryScaffold(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := s.getMemoryRepoPath()
	if path == "" {
		http.Redirect(w, r, "/?scaffold_err="+url.QueryEscape("memory repo path is not configured"), http.StatusSeeOther)
		return
	}
	missing, err := memory.MissingItems(path)
	if err != nil {
		http.Redirect(w, r, "/?scaffold_err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if len(missing) == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := memory.CreateMissing(path, missing); err != nil {
		http.Redirect(w, r, "/?scaffold_err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	target := "/?scaffold_created=" + strconv.Itoa(len(missing))
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleProcRestart is POST /procs/{name}/restart - invokes the manager's
// manual restart, clearing its circuit breaker. A missing callback is a
// no-op (the manager isn't up yet) and the user is redirected either way so
// the updated status flows through SSE without a blank page.
func (s *Server) handleProcRestart(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if fn := s.getProcRestart(name); fn != nil {
			fn()
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}
