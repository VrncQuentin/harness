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
	"github.com/vrnc/harness/internal/config"
	gitw "github.com/vrnc/harness/internal/git"
	"github.com/vrnc/harness/internal/index"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/proc"
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

	rt.reqQueue = queue.New(3, "", nil)
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
	rt.reqQueue = queue.New(1, "", nil)
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
	ad := &uiAgentRegistryAdapter{reg: reg, mem: mem}

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
	ad := &uiAgentRegistryAdapter{reg: reg, mem: mem}

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

func TestTaskRunnerRoutesThroughAssemblerAndQueue(t *testing.T) {
	root := t.TempDir()
	for rel, body := range map[string]string{
		"global/rules.md":                "phase2 global rules",
		"global/user.md":                 "phase2 user profile",
		"global/facts.md":                "phase2 fact",
		"agents/coder/persona.md":        "phase2 coder persona",
		"agents/coder/rules.md":          "phase2 coder rules",
		"agents/coder/notes.md":          "phase2 coder notes",
		"projects/global/rules.md":       "phase2 project rules",
		"projects/global/sessions.jsonl": "",
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
	q := queue.New(1, "", queued)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("queue Start: %v", err)
	}
	defer q.Stop()

	rt := New(cfg, nil, LogRings{})
	rt.memReader = mem
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

	root := initRuntimeProjectRepo(t, true)
	cfg := config.Defaults()
	cfg.Project.ActiveProjectSlug = "global"
	cfg.Model.Port = modelPort
	cfg.Embedder.Port = freeTCPPort(t)

	rt := New(cfg, nil, LogRings{})
	rt.memReader = memory.NewLayoutV2Reader(root, "global", root)

	mgr, adapter := rt.buildSessionManager(nil, ui.NewServer(0), projectRepoRoots{
		globalRoot: root,
		activeRoot: root,
		activeSlug: "global",
	})
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

func initRuntimeProjectRepo(t *testing.T, global bool) string {
	t.Helper()
	root := t.TempDir()
	repo, err := gitw.Init(root)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := memory.CreateMissingProjectRepo(root, global); err != nil {
		t.Fatalf("scaffold project repo: %v", err)
	}
	if _, err := repo.Commit(gitw.BuildMessage(map[string]string{"type": "scaffold"}, "initialize project memory repo"), memory.ProjectRepoScaffoldFiles(global)); err != nil {
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
	episodePath := filepath.Join(root, "projects", "global", "episodes", "coder", "ep1.md")
	if err := os.MkdirAll(filepath.Dir(episodePath), 0o755); err != nil {
		t.Fatalf("MkdirAll episode dir: %v", err)
	}
	if err := os.WriteFile(episodePath, []byte("episode body"), 0o644); err != nil {
		t.Fatalf("WriteFile episode: %v", err)
	}
	indexDir := filepath.Join(root, "projects", "global", "index", "_episodes")
	called := false
	rb := &indexRebuilder{
		mem:      memory.NewDirReader(root),
		emb:      stubEmbedder{vec: []float32{1, 0}},
		indexDir: indexDir,
		slug:     "global",
		onRebuilt: func(idx *index.Index) {
			called = true
			if !idx.Contains("ep1") {
				t.Errorf("rebuilt index missing ep1")
			}
		},
	}

	if err := rb.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if rb.idx == nil {
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
