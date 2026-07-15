package runtime

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/agentloop"
	"github.com/vrnc/harness/internal/approvals"
	"github.com/vrnc/harness/internal/config"
	gitw "github.com/vrnc/harness/internal/git"
	"github.com/vrnc/harness/internal/index"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/memoryops"
	"github.com/vrnc/harness/internal/proc"
	"github.com/vrnc/harness/internal/project"
	"github.com/vrnc/harness/internal/prompt"
	"github.com/vrnc/harness/internal/queue"
	"github.com/vrnc/harness/internal/tools"
	"github.com/vrnc/harness/internal/ui"
)

func TestNewStoresInitialConfig(t *testing.T) {
	cfg := config.Defaults()
	cfg.Agent.Active = "coder"

	rt := New(cfg, nil, LogRings{})

	if got := rt.getActiveAgent(); got != "coder" {
		t.Fatalf("active agent = %q, want coder", got)
	}
}

func TestNewEventChannelUsesRuntimeBuffer(t *testing.T) {
	ch := NewEventChannel()

	if cap(ch) != EventBufferSize {
		t.Fatalf("event channel cap = %d, want %d", cap(ch), EventBufferSize)
	}
}

func TestQueueStatsReportsLiveQueueDepthAndCapacity(t *testing.T) {
	rt := New(config.Defaults(), nil, LogRings{})
	depth, capacity := rt.QueueStats()
	if depth != 0 || capacity != 0 {
		t.Fatalf("empty QueueStats = %d/%d, want 0/0", depth, capacity)
	}

	rt.reqQueue = queue.New(3, nil)
	for _, id := range []string{"one", "two"} {
		if err := rt.reqQueue.Enqueue(queue.Request{ID: id, Response: make(chan inference.Token, 1), Ctx: context.Background()}); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}

	depth, capacity = rt.QueueStats()
	if depth != 2 || capacity != 3 {
		t.Fatalf("QueueStats = %d/%d, want 2/3", depth, capacity)
	}
}
func TestRestartCallbacksTolerateMissingManagers(t *testing.T) {
	rt := New(config.Defaults(), nil, LogRings{})

	rt.RestartLlama()
	rt.RestartEmbedder()
}

func TestStartMemoryAndAPIInvalidRepoDoesNotBindAPI(t *testing.T) {
	port := freeTCPPort(t)
	cfg := config.Defaults()
	cfg.API.Enabled = true
	cfg.API.Port = port

	rt := New(cfg, nil, LogRings{})
	rt.reqQueue = queue.New(1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt.startMemoryAndAPI(ctx, ui.NewServer(0), nil)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("API bound despite invalid memory repo: %v", err)
	}
	_ = ln.Close()
}

func TestPushStatusNilManagerSkipsSetter(t *testing.T) {
	called := false
	pushStatus(nil, "llama-server", func(ui.ProcessStatus) {
		called = true
	})
	if called {
		t.Fatalf("setter invoked for nil manager")
	}
}

func TestPushStatusPopulatesStatusFromManager(t *testing.T) {
	mgr := proc.NewManager(proc.ManagerConfig{Name: "llama-server"})

	var got ui.ProcessStatus
	pushStatus(mgr, "llama-server", func(st ui.ProcessStatus) {
		got = st
	})

	if got.Name != "llama-server" {
		t.Errorf("Name = %q, want llama-server", got.Name)
	}
	// A freshly-built manager has zero state for the rest; we only assert
	// that pushStatus copied the snapshot through, not the manager's logic.
	if got.Running || got.Healthy || got.Failed || got.RestartCount != 0 {
		t.Errorf("fresh manager produced non-zero status: %+v", got)
	}
}

func TestUIAgentRegistryAdapterListMatchesGet(t *testing.T) {
	mem := newMemoryRepo(t, map[string]string{
		"agents/coder/persona.md":    "coder persona",
		"agents/coder/rules.md":      "coder rules",
		"agents/coder/notes.md":      "coder notes",
		"agents/reviewer/persona.md": "reviewer persona",
	})
	var active string
	reg := agent.NewDiskRegistry(mem,
		func() string { return active },
		func(name string) error { active = name; return nil },
	)
	ad := &uiAgentRegistryAdapter{reg: reg, globalMem: mem, activeMem: mem, getProjectSlug: func() string { return "global" }}

	list, err := ad.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List returned %d agents, want 2", len(list))
	}

	for _, listed := range list {
		fromGet, err := ad.Get(listed.Name)
		if err != nil {
			t.Errorf("Get(%q): %v", listed.Name, err)
			continue
		}
		if !reflect.DeepEqual(listed, fromGet) {
			t.Errorf("List entry for %q diverges from Get:\n list = %+v\n  get = %+v",
				listed.Name, listed, fromGet)
		}
	}
}

func TestUIAgentRegistryAdapterListTreatsMissingFilesAsEmpty(t *testing.T) {
	// Only persona.md is on disk; rules and notes are absent. The adapter
	// must surface the agent with empty Rules/Notes rather than skip it.
	mem := newMemoryRepo(t, map[string]string{
		"agents/coder/persona.md": "P",
	})
	var active string
	reg := agent.NewDiskRegistry(mem,
		func() string { return active },
		func(name string) error { active = name; return nil },
	)
	ad := &uiAgentRegistryAdapter{reg: reg, globalMem: mem, activeMem: mem, getProjectSlug: func() string { return "global" }}

	list, err := ad.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != "coder" {
		t.Fatalf("List = %+v, want one agent named coder", list)
	}
	if list[0].Persona != "P" {
		t.Errorf("Persona = %q, want P", list[0].Persona)
	}
	if list[0].Rules != "" {
		t.Errorf("Rules = %q, want empty (file missing)", list[0].Rules)
	}
	if list[0].Notes != "" {
		t.Errorf("Notes = %q, want empty (file missing)", list[0].Notes)
	}
}

func TestUIAgentRegistryAdapterUsesActiveProjectNotes(t *testing.T) {
	global := newMemoryRepo(t, map[string]string{
		"agents/coder/persona.md": "global persona",
		"agents/coder/rules.md":   "global rules",
		"agents/coder/notes.md":   "global notes",
	})
	active := newMemoryRepo(t, map[string]string{
		"agents/coder/notes.md": "project notes",
	})
	reg := agent.NewDiskRegistry(global, func() string { return "coder" }, func(string) error { return nil })
	ad := &uiAgentRegistryAdapter{reg: reg, globalMem: global, activeMem: active, getProjectSlug: func() string { return "dt" }}

	info, err := ad.Get("coder")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Persona != "global persona" || info.Rules != "global rules" {
		t.Fatalf("definition fallback changed: %+v", info)
	}
	if info.Notes != "project notes" {
		t.Fatalf("Notes = %q, want project notes", info.Notes)
	}

	if err := ad.WriteNotes("coder", []byte("new project notes")); err != nil {
		t.Fatalf("WriteNotes: %v", err)
	}
	got, err := active.Read("agents/coder/notes.md")
	if err != nil {
		t.Fatalf("read active notes: %v", err)
	}
	if string(got) != "new project notes" {
		t.Fatalf("active notes = %q", string(got))
	}
	globalNotes, err := global.Read("agents/coder/notes.md")
	if err != nil {
		t.Fatalf("read global notes: %v", err)
	}
	if string(globalNotes) != "global notes" {
		t.Fatalf("global notes changed to %q", string(globalNotes))
	}
}
func TestTaskRunnerApprovalEvaluatorsDoNotShareSessionRules(t *testing.T) {
	ad := &taskRunnerAdapter{approvalLayers: []approvals.Layer{approvals.DefaultLayer()}}

	first := ad.newApprovalEvaluator()
	second := ad.newApprovalEvaluator()
	if first == nil || second == nil {
		t.Fatal("expected approval evaluators")
	}

	first.AddSessionRule(approvals.Rule{
		ToolID:   "file_write",
		Decision: approvals.Allowed,
		Source:   "session: always allowed",
	})

	if got, _ := first.Evaluate("file_write", ""); got != approvals.Allowed {
		t.Fatalf("first evaluator decision = %v, want Allowed", got)
	}
	if got, _ := second.Evaluate("file_write", ""); got != approvals.Ask {
		t.Fatalf("second evaluator decision = %v, want Ask without first session rule", got)
	}
}
func TestTaskRunnerCancelTaskCancelsActiveEngine(t *testing.T) {
	called := false
	ad := &taskRunnerAdapter{
		cancels: map[string]context.CancelFunc{
			"task-1": func() { called = true },
		},
	}
	if err := ad.CancelTask("task-1"); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if !called {
		t.Fatal("cancel func was not called")
	}
	if err := ad.CancelTask("missing"); err == nil {
		t.Fatal("expected missing task to return an error")
	}
}
func TestTaskRunnerRecordsPartialTranscriptOnCancel(t *testing.T) {
	root := initRuntimeProjectRepo(t)
	cfg := config.Defaults()
	cfg.Project.ActiveProjectSlug = "global"
	rt := New(cfg, nil, LogRings{})
	rt.inferClient = blockingInferenceClient{token: inference.Token{Content: "partial answer"}}

	mgr, _ := rt.buildSessionManagerWithClients(nil, ui.NewServer(0), projectRepoRoots{
		globalRoot: root,
		activeRoot: root,
		activeSlug: "global",
	}, rt.ensureInferenceClient(), nil)
	rt.setSessionManager(mgr)

	ad := &taskRunnerAdapter{rt: rt, registry: tools.NewRegistry()}
	id, evch, err := ad.RunTask(context.Background(), "coder", "", []ui.ChatMessage{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	select {
	case ev, ok := <-evch:
		if !ok {
			t.Fatal("event channel closed before text event")
		}
		if ev.Type != agentloop.EvtText {
			t.Fatalf("first event = %s, want text", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for text event")
	}
	if err := ad.CancelTask(id); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	for range evch {
	}

	snap := mgr.Snapshot(id)
	if snap == nil {
		t.Fatal("session snapshot missing")
	}
	for _, msg := range snap.Conversation {
		if msg.Role == "assistant" && msg.Content == "partial answer" {
			return
		}
	}
	t.Fatalf("partial assistant text was not recorded; conversation=%+v", snap.Conversation)
}

func TestRecordTaskEventsPairsApprovalAuditNumbers(t *testing.T) {
	root := initRuntimeProjectRepo(t)
	cfg := config.Defaults()
	cfg.Project.ActiveProjectSlug = "global"
	rt := New(cfg, nil, LogRings{})
	mgr, _ := rt.buildSessionManagerWithClients(nil, ui.NewServer(0), projectRepoRoots{
		globalRoot: root,
		activeRoot: root,
		activeSlug: "global",
	}, rt.ensureInferenceClient(), nil)
	if mgr == nil {
		t.Fatal("buildSessionManager returned nil")
	}
	s := mgr.Start("coder")

	recordTaskEvents(mgr, s.ID, []agentloop.Event{
		{Type: agentloop.EvtApprovalNeeded, ApprovalID: "approval-1", ToolID: "file_write", ToolArgs: `{"path":"x"}`},
		{Type: agentloop.EvtApproval, ApprovalID: "approval-1", ToolID: "file_write"},
	})

	snap := mgr.Snapshot(s.ID)
	if snap == nil {
		t.Fatal("session snapshot missing")
	}
	var approvals []string
	for _, msg := range snap.Conversation {
		if msg.Role == "system" && msg.Name == "approval" {
			approvals = append(approvals, msg.Content)
		}
	}
	if len(approvals) != 2 {
		t.Fatalf("approval audit messages = %d, want 2; conversation=%+v", len(approvals), snap.Conversation)
	}
	if !strings.Contains(approvals[0], "[approval_needed #1]") || !strings.Contains(approvals[1], "[approval #1]") {
		t.Fatalf("approval audit numbers not paired: %#v", approvals)
	}
	if strings.Contains(approvals[1], "#2") {
		t.Fatalf("approval result consumed a new number: %#v", approvals)
	}
}

func TestTaskRunnerDoesNotDuplicateSingleMessageOnResume(t *testing.T) {
	root := initRuntimeProjectRepo(t)
	cfg := config.Defaults()
	cfg.Project.ActiveProjectSlug = "global"
	rt := New(cfg, nil, LogRings{})
	rt.inferClient = &capturingInferenceClient{tokens: []inference.Token{{Content: "ok"}, {Done: true}}}

	mgr, _ := rt.buildSessionManagerWithClients(nil, ui.NewServer(0), projectRepoRoots{
		globalRoot: root,
		activeRoot: root,
		activeSlug: "global",
	}, rt.ensureInferenceClient(), nil)
	rt.setSessionManager(mgr)

	s := mgr.Start("coder")
	if err := mgr.Append(s.ID, inference.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatalf("seed Append: %v", err)
	}

	ad := &taskRunnerAdapter{rt: rt, registry: tools.NewRegistry()}
	_, evch, err := ad.RunTask(context.Background(), "coder", s.ID, []ui.ChatMessage{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	for range evch {
	}

	snap := mgr.Snapshot(s.ID)
	if snap == nil {
		t.Fatal("session snapshot missing")
	}
	userTurns := 0
	for _, msg := range snap.Conversation {
		if msg.Role == "user" && msg.Content == "hello" {
			userTurns++
		}
	}
	if userTurns != 1 {
		t.Fatalf("user turn count = %d, want 1; conversation=%+v", userTurns, snap.Conversation)
	}
}
func TestTaskRunnerDoesNotUseMemoryRepoAsSandboxFallback(t *testing.T) {
	root := t.TempDir()
	secretPath := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("secret memory contents"), 0o644); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}

	cfg := config.Defaults()
	cfg.Agent.Active = "coder"
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	client := &sequenceInferenceClient{sequences: [][]inference.Token{
		{
			{ToolCallDelta: &inference.ToolCallDelta{
				Index:     0,
				ID:        "call-1",
				Name:      "file_read",
				Arguments: fmt.Sprintf("{\"path\":%q}", secretPath),
			}},
			{Done: true},
		},
		{
			{Content: "done"},
			{Done: true},
		},
	}}

	rt := New(cfg, nil, LogRings{})
	rt.inferClient = client
	rt.projectStore = &runtimeProjectStoreStub{
		projects: map[string]project.Project{
			project.GlobalSlug: {
				Slug:           project.GlobalSlug,
				DisplayName:    "Global",
				MemoryRepoPath: root,
			},
		},
		dirs: map[string][]project.Directory{
			project.GlobalSlug: nil,
		},
	}

	registry := tools.NewRegistry()
	if err := tools.RegisterBuiltins(registry); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	ad := &taskRunnerAdapter{rt: rt, registry: registry}

	_, evch, err := ad.RunTask(context.Background(), "coder", "", []ui.ChatMessage{{Role: "user", Content: "read the file"}})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	sawFileRead := false
	for ev := range evch {
		if ev.Type != agentloop.EvtToolResult || ev.ToolID != "file_read" {
			continue
		}
		sawFileRead = true
		if !strings.Contains(ev.ToolError, "sandbox") {
			t.Fatalf("file_read ToolError = %q, want sandbox error", ev.ToolError)
		}
		if strings.Contains(ev.ToolResult, "secret memory contents") {
			t.Fatalf("file_read returned memory repo content despite no configured project directories: %q", ev.ToolResult)
		}
	}
	if !sawFileRead {
		t.Fatal("did not observe file_read tool result")
	}
}
func TestTaskRunnerRoutesThroughAssemblerAndQueue(t *testing.T) {
	root := t.TempDir()
	for rel, body := range map[string]string{
		"rules.md":                "phase2 global rules",
		"user.md":                 "phase2 user profile",
		"facts.md":                "phase2 fact",
		"agents/coder/persona.md": "phase2 coder persona",
		"agents/coder/rules.md":   "phase2 coder rules",
		"agents/coder/notes.md":   "phase2 coder notes",
		"projects/rules.md":       "phase2 project rules",
		"sessions.jsonl":          "",
	} {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", abs, err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", abs, err)
		}
	}

	cfg := config.Defaults()
	cfg.Agent.Active = "coder"
	cfg.Project.ActiveProjectSlug = "global"
	mem := memory.NewDirReader(root)
	active := "coder"
	reg := agent.NewDiskRegistry(mem,
		func() string { return active },
		func(name string) error { active = name; return nil },
	)

	queued := &capturingInferenceClient{tokens: []inference.Token{{Content: "ok"}, {Done: true}}}
	q := queue.New(1, queued)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("queue Start: %v", err)
	}
	defer q.Stop()

	rt := New(cfg, nil, LogRings{})
	rt.globalMem = mem
	rt.activeMem = mem
	rt.agentReg = reg
	rt.assembler = prompt.NewDiskAssembler(mem, reg, cfg.Prompt).WithProjectSlug("global")
	rt.inferClient = failingInferenceClient{err: fmt.Errorf("direct inference path used")}

	ad := &taskRunnerAdapter{
		rt:       rt,
		registry: tools.NewRegistry(),
		asm:      &apiAssemblerAdapter{rt: rt},
		q:        q,
	}
	_, evch, err := ad.RunTask(ctx, "coder", "", []ui.ChatMessage{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	var text strings.Builder
	for ev := range evch {
		if ev.Type == agentloop.EvtText {
			text.WriteString(ev.Content)
		}
		if ev.Type == agentloop.EvtError {
			t.Fatalf("unexpected task error event: %s", ev.Content)
		}
	}
	if text.String() != "ok" {
		t.Fatalf("task text = %q, want ok", text.String())
	}

	queued.mu.Lock()
	defer queued.mu.Unlock()
	if queued.calls != 1 {
		t.Fatalf("queued inference calls = %d, want 1", queued.calls)
	}
	joined := messagesText(queued.last.Messages)
	for _, want := range []string{"phase2 global rules", "phase2 coder persona", "hello"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("assembled queued messages missing %q:\n%s", want, joined)
		}
	}
}

func TestBuildSessionManagerUsesPhysicalProjectRepoPaths(t *testing.T) {
	modelPort, shutdownModel := startFakeModelServer(t, "runtime summary")
	defer shutdownModel()

	root := initRuntimeProjectRepo(t)
	cfg := config.Defaults()
	cfg.Project.ActiveProjectSlug = "global"
	cfg.Model.Port = modelPort
	cfg.Embedder.Port = freeTCPPort(t)

	rt := New(cfg, nil, LogRings{})
	rt.globalMem = memory.NewDirReader(root)
	rt.activeMem = rt.globalMem

	mgr, adapter := rt.buildSessionManagerWithClients(nil, ui.NewServer(0), projectRepoRoots{
		globalRoot: root,
		activeRoot: root,
		activeSlug: "global",
	}, rt.ensureInferenceClient(), nil)
	if mgr == nil || adapter == nil {
		t.Fatal("buildSessionManager returned nil manager")
	}

	s := mgr.Start("coder")
	if err := mgr.Append(s.ID, inference.Message{Role: "user", Content: "save through runtime wiring"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := mgr.Save(ctx, s.ID)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if res.EpisodePath != "episodes/coder/"+s.ID+".md" {
		t.Fatalf("EpisodePath = %q", res.EpisodePath)
	}
	for _, rel := range []string{
		"episodes/coder/" + s.ID + ".md",
		"episodes/coder/" + s.ID + ".json",
		"sessions.jsonl",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("expected %s to exist: %v", rel, err)
		}
	}
}

func initRuntimeProjectRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	repo, err := gitw.Init(root)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := memory.CreateMissingProjectRepo(root, true); err != nil {
		t.Fatalf("scaffold project repo: %v", err)
	}
	if _, err := repo.Commit(gitw.BuildMessage(map[string]string{"type": "scaffold"}, "initialize project memory repo"), memory.ProjectRepoScaffoldFiles(true)); err != nil {
		t.Fatalf("commit scaffold: %v", err)
	}
	return root
}

func startFakeModelServer(t *testing.T, summary string) (int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake model: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n", summary)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	port := ln.Addr().(*net.TCPAddr).Port
	return port, func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}
func TestIndexRebuilderCreatesMissingEpisodeIndex(t *testing.T) {
	root := t.TempDir()
	episodePath := filepath.Join(root, "episodes", "coder", "ep1.md")
	if err := os.MkdirAll(filepath.Dir(episodePath), 0o755); err != nil {
		t.Fatalf("MkdirAll episode dir: %v", err)
	}
	if err := os.WriteFile(episodePath, []byte("episode body"), 0o644); err != nil {
		t.Fatalf("WriteFile episode: %v", err)
	}
	indexDir := filepath.Join(root, "index", "_episodes")
	called := false
	rb := &memoryops.EpisodeRebuilder{
		Mem:      memory.NewDirReader(root),
		Embedder: stubEmbedder{vec: []float32{1, 0}},
		IndexDir: indexDir,
		Slug:     "global",
		OnRebuilt: func(idx *index.Index) {
			called = true
			if !idx.Contains("ep1") {
				t.Errorf("rebuilt index missing ep1")
			}
		},
	}

	if err := rb.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if rb.Index == nil {
		t.Fatal("rebuilder did not retain created index")
	}
	if !called {
		t.Fatal("onRebuilt callback was not called")
	}
	opened, err := index.Open(indexDir)
	if err != nil {
		t.Fatalf("Open rebuilt index: %v", err)
	}
	if !opened.Contains("ep1") {
		t.Fatal("rebuilt index does not contain ep1")
	}
}

type stubEmbedder struct {
	vec []float32
}

func (s stubEmbedder) Embed(_ context.Context, chunks []string) ([][]float32, error) {
	out := make([][]float32, len(chunks))
	for i := range out {
		out[i] = append([]float32(nil), s.vec...)
	}
	return out, nil
}

func (s stubEmbedder) Health(context.Context) error { return nil }

type capturingInferenceClient struct {
	mu     sync.Mutex
	tokens []inference.Token
	calls  int
	last   inference.CompletionRequest
}

func (c *capturingInferenceClient) Complete(_ context.Context, req inference.CompletionRequest) (<-chan inference.Token, error) {
	c.mu.Lock()
	c.calls++
	c.last = req
	tokens := append([]inference.Token(nil), c.tokens...)
	c.mu.Unlock()

	ch := make(chan inference.Token, len(tokens))
	for _, tok := range tokens {
		ch <- tok
	}
	close(ch)
	return ch, nil
}

func (c *capturingInferenceClient) Health(context.Context) error { return nil }

type blockingInferenceClient struct {
	token inference.Token
}

func (c blockingInferenceClient) Complete(ctx context.Context, _ inference.CompletionRequest) (<-chan inference.Token, error) {
	ch := make(chan inference.Token, 1)
	go func() {
		defer close(ch)
		ch <- c.token
		<-ctx.Done()
	}()
	return ch, nil
}

func (c blockingInferenceClient) Health(context.Context) error { return nil }

type sequenceInferenceClient struct {
	mu        sync.Mutex
	sequences [][]inference.Token
	calls     int
}

func (c *sequenceInferenceClient) Complete(_ context.Context, req inference.CompletionRequest) (<-chan inference.Token, error) {
	c.mu.Lock()
	idx := c.calls
	c.calls++
	var tokens []inference.Token
	if idx < len(c.sequences) {
		tokens = append([]inference.Token(nil), c.sequences[idx]...)
	} else {
		tokens = []inference.Token{{Done: true}}
	}
	c.mu.Unlock()

	ch := make(chan inference.Token, len(tokens))
	for _, tok := range tokens {
		ch <- tok
	}
	close(ch)
	return ch, nil
}

func (c *sequenceInferenceClient) Health(context.Context) error { return nil }

type runtimeProjectStoreStub struct {
	projects map[string]project.Project
	dirs     map[string][]project.Directory
}

func (s *runtimeProjectStoreStub) List(bool) ([]project.Project, error) {
	projects := make([]project.Project, 0, len(s.projects))
	for _, p := range s.projects {
		projects = append(projects, p)
	}
	return projects, nil
}

func (s *runtimeProjectStoreStub) Get(slug string) (project.Project, error) {
	p, ok := s.projects[slug]
	if !ok {
		return project.Project{}, project.ErrNotFound
	}
	return p, nil
}

func (s *runtimeProjectStoreStub) Create(project.CreateInput) (project.Project, error) {
	return project.Project{}, nil
}

func (s *runtimeProjectStoreStub) Update(project.UpdateInput) (project.Project, error) {
	return project.Project{}, nil
}

func (s *runtimeProjectStoreStub) SetHidden(string, bool) error { return nil }

func (s *runtimeProjectStoreStub) ListDirectories(slug string) ([]project.Directory, error) {
	return append([]project.Directory(nil), s.dirs[slug]...), nil
}

func (s *runtimeProjectStoreStub) AddDirectory(string, string) error { return nil }

func (s *runtimeProjectStoreStub) RemoveDirectory(string, string) error { return nil }

type failingInferenceClient struct {
	err error
}

func (f failingInferenceClient) Complete(context.Context, inference.CompletionRequest) (<-chan inference.Token, error) {
	return nil, f.err
}

func (f failingInferenceClient) Health(context.Context) error { return f.err }

func messagesText(msgs []inference.Message) string {
	var b strings.Builder
	for _, msg := range msgs {
		b.WriteString(msg.Role)
		b.WriteByte(':')
		b.WriteString(msg.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

// newMemoryRepo creates a temp directory populated with files (relative paths
// using forward slashes) and returns a memory.DirReader rooted at it.
func newMemoryRepo(t *testing.T, files map[string]string) *memory.DirReader {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", abs, err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", abs, err)
		}
	}
	return memory.NewDirReader(root)
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on ephemeral port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr is %T, want *net.TCPAddr", ln.Addr())
	}
	return addr.Port
}
