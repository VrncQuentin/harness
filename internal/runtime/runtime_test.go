package runtime

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VrncQuentin/harness/internal/agent"
	"github.com/VrncQuentin/harness/internal/agentloop"
	"github.com/VrncQuentin/harness/internal/approvals"
	"github.com/VrncQuentin/harness/internal/config"
	gitw "github.com/VrncQuentin/harness/internal/git"
	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/pathid"
	"github.com/VrncQuentin/harness/internal/proc"
	"github.com/VrncQuentin/harness/internal/project"
	"github.com/VrncQuentin/harness/internal/prompt"
	"github.com/VrncQuentin/harness/internal/queue"
	"github.com/VrncQuentin/harness/internal/session"
	"github.com/VrncQuentin/harness/internal/tools"
	"github.com/VrncQuentin/harness/internal/ui"
)

// linkDir creates a directory link at link pointing at target, preferring a
// symlink and falling back to a Windows junction.
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	} else if runtime.GOOS != "windows" {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("cannot create directory link: %v: %s", err, out)
	}
}

// newSessionManagerForTest opens the active reader the way one runtime
// generation does and builds a session manager wired to it, publishing a
// generation that immediately owns the reader. The session manager is NOT
// registered on the generation: only tests that exercise the shutdown flush or
// reload quiesce wire it explicitly (rt.gen.sessionMgr = mgr), so a test that
// just uses the manager directly never triggers a shutdown flush. Shutdown's
// release closes the reader, so the test must not register its own cleanup
// (cleanup runs LIFO after Shutdown's flush would have already used the
// reader).
func newSessionManagerForTest(t *testing.T, rt *Runtime, root string, infClient inference.Client) (*gitw.Repo, *session.Manager, *uiSessionStoreAdapter) {
	t.Helper()
	reader, err := memory.NewDirReader(root)
	if err != nil {
		t.Fatal(err)
	}
	repo, mgr, adapter, err := rt.buildSessionManagerWithClients(nil, projectRepoRoots{
		globalRoot: root,
		activeRoot: root,
		activeSlug: project.GlobalSlug,
	}, infClient, nil, nil, reader, project.GlobalSlug)
	if err != nil {
		t.Fatal(err)
	}
	rt.gen = &generation{globalMem: reader, activeMem: reader}
	rt.gen.acquire()
	return repo, mgr, adapter
}

func TestNewStoresInitialConfig(t *testing.T) {
	cfg := config.Defaults()
	cfg.Agent.Active = "coder"

	rt := New(cfg, nil, LogRings{})
	t.Cleanup(func() { stopRuntime(t, rt) })

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

func TestEffectiveModelForUsesActiveProjectOverrides(t *testing.T) {
	cfg := config.Defaults()
	cfg.Model.Binary = "global-llama"
	cfg.Model.ModelPath = "global.gguf"
	cfg.Model.CtxSize = 2048
	cfg.Model.GPULayers = 1
	cfg.Model.NParallel = 1
	cfg.Model.Port = 12345
	cfg.Model.CacheTypeK = "q8_0"
	cfg.Model.CacheTypeV = "q8_0"
	cfg.Project.ActiveProjectSlug = "demo"

	projectBinary := "project-llama"
	projectModel := "project.gguf"
	projectCtx := 4096
	projectGPU := 9
	projectParallel := 3
	rt := New(cfg, nil, LogRings{})
	t.Cleanup(func() { stopRuntime(t, rt) })
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		"demo": {
			Slug:           "demo",
			DisplayName:    "Demo",
			MemoryRepoPath: t.TempDir(),
			ModelBinary:    &projectBinary,
			ModelPath:      &projectModel,
			ModelCtxSize:   &projectCtx,
			ModelGPULayers: &projectGPU,
			ModelNParallel: &projectParallel,
		},
	}}

	model := rt.effectiveModelFor(&cfg)
	if model.Binary != projectBinary || model.ModelPath != projectModel {
		t.Fatalf("effective binary/model = %q/%q, want project override %q/%q", model.Binary, model.ModelPath, projectBinary, projectModel)
	}
	if model.CtxSize != projectCtx || model.GPULayers != projectGPU || model.NParallel != projectParallel {
		t.Fatalf("effective numeric overrides = ctx %d gpu %d parallel %d", model.CtxSize, model.GPULayers, model.NParallel)
	}
	if model.Port != cfg.Model.Port || model.CacheTypeK != cfg.Model.CacheTypeK || model.CacheTypeV != cfg.Model.CacheTypeV {
		t.Fatalf("effective global-only fields changed unexpectedly: %+v", model)
	}
}

func TestPromptConfigForUsesRunningModelCtx(t *testing.T) {
	cfg := config.Defaults()
	cfg.Model.CtxSize = 2048
	cfg.Prompt.CtxSize = 9999
	cfg.Project.ActiveProjectSlug = "demo"

	projectCtx := 4096
	rt := New(cfg, nil, LogRings{})
	t.Cleanup(func() { stopRuntime(t, rt) })
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		"demo": {
			Slug:           "demo",
			DisplayName:    "Demo",
			MemoryRepoPath: t.TempDir(),
			ModelCtxSize:   &projectCtx,
		},
	}}

	// The running model for a generation is the effective model; the prompt
	// context ceiling must track it (project ctx override flows through).
	effective := rt.effectiveModelFor(&cfg)
	if effective.CtxSize != projectCtx {
		t.Fatalf("effective model ctx = %d, want project override %d", effective.CtxSize, projectCtx)
	}
	promptCfg := promptConfigFor(&cfg, effective)
	if promptCfg.CtxSize != projectCtx {
		t.Fatalf("prompt ctx = %d, want running model ctx %d", promptCfg.CtxSize, projectCtx)
	}
	if promptCfg.MemoryTokenBudget != cfg.Prompt.MemoryTokenBudget {
		t.Fatalf("prompt config was not otherwise preserved: %+v", promptCfg)
	}
}
func TestLlamaArgsForModelUsesEffectiveModelFields(t *testing.T) {
	model := config.Defaults().Model
	model.Binary = "llama-bin"
	model.ModelPath = "project.gguf"
	model.CtxSize = 4096
	model.GPULayers = 7
	model.NParallel = 2
	model.Port = 8123
	model.Verbose = true
	model.CacheTypeK = "q4_0"
	model.CacheTypeV = "f16"

	bin, args := llamaArgsForModel(model)
	if bin != model.Binary {
		t.Fatalf("binary = %q, want %q", bin, model.Binary)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{model.ModelPath, "4096", "7", "2", "8123", "--verbose", "q4_0", "f16"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("llama args %q missing %q", joined, want)
		}
	}
	if got := llamaHealthURL(model); got != "http://127.0.0.1:8123/health" {
		t.Fatalf("health URL = %q", got)
	}
}

func TestEmbedderArgsForConfig(t *testing.T) {
	embed := config.Defaults().Embedder
	embed.Binary = "embed-bin"
	embed.ModelPath = "embed.gguf"
	embed.Port = 8124
	embed.Verbose = true

	bin, args := embedderArgsForConfig(embed)
	if bin != embed.Binary {
		t.Fatalf("binary = %q, want %q", bin, embed.Binary)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{embed.ModelPath, "--embedding", "--n-gpu-layers 0", "8124", "--verbose"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("embedder args %q missing %q", joined, want)
		}
	}
	if got := embedderHealthURL(embed); got != "http://127.0.0.1:8124/health" {
		t.Fatalf("health URL = %q", got)
	}
}

func TestModelProcessArgBuildersKeepCacheAndVerbosity(t *testing.T) {
	model := config.Defaults().Model
	model.Binary = "llama-bin"
	model.ModelPath = "project.gguf"
	model.CtxSize = 4096
	model.GPULayers = 7
	model.NParallel = 2
	model.Port = 8123
	model.Verbose = false
	model.CacheTypeK = "q4_0"
	model.CacheTypeV = "f16"

	_, args := llamaArgsForModel(model)
	if hasRuntimeVerbose(args) {
		t.Fatalf("--verbose must not appear when verbose=false: %v", args)
	}
	for flag, want := range map[string]string{"--cache-type-k": "q4_0", "--cache-type-v": "f16"} {
		if got := runtimeFlagValue(args, flag); got != want {
			t.Fatalf("%s = %q, want %q (args=%v)", flag, got, want, args)
		}
	}
}

func runtimeFlagValue(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func hasRuntimeVerbose(args []string) bool {
	for _, a := range args {
		if a == "--verbose" {
			return true
		}
	}
	return false
}
func TestQueueStatsReportsLiveQueueDepthAndCapacity(t *testing.T) {
	rt := New(config.Defaults(), nil, LogRings{})
	depth, capacity := rt.QueueStats()
	if depth != 0 || capacity != 0 {
		t.Fatalf("empty QueueStats = %d/%d, want 0/0", depth, capacity)
	}

	rt.reqQueue = queue.New(3, nil)
	for _, id := range []string{"one", "two"} {
		if err := rt.reqQueue.Enqueue(queue.Request{Response: make(chan inference.Token, 1), Ctx: context.Background()}); err != nil {
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

func TestApplyConfigFailedMemoryReloadRestoresExistingServices(t *testing.T) {
	root := initRuntimeProjectRepo(t)
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	loaded := cfg
	loaded.Project.ActiveProjectSlug = "missing"

	mem, err := memory.NewDirReader(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	rt := New(cfg, &runtimeConfigStore{cfg: &loaded, saved: true}, LogRings{})
	t.Cleanup(func() { stopRuntime(t, rt) })
	rt.started = true
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
	}}

	uiServer := ui.NewServer(0)
	// The existing generation carries the published UI snapshot and the
	// readable reader; a failed reload must not replace it.
	rt.gen = &generation{globalMem: mem, activeMem: mem, uiSnap: ui.ServiceDeps{MemoryRepoPath: root, MemoryStore: mem}}
	rt.gen.acquire()

	result := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil)
	if result.LiveApplied {
		t.Fatal("failed memory/API reload should not report live apply")
	}
	if rt.gen == nil || rt.gen.globalMem != mem || rt.gen.activeMem != mem {
		t.Fatal("runtime memory repos were not restored after failed reload")
	}
	deps, release := rt.AcquireUISnapshot()
	defer release()
	if deps.MemoryRepoPath != root || deps.MemoryStore != mem {
		t.Fatalf("UI service deps were not restored: path=%q store=%T", deps.MemoryRepoPath, deps.MemoryStore)
	}
}

func TestApplyConfigRetriesMissingMemoryServicesWithoutConfigChange(t *testing.T) {
	root := initRuntimeProjectRepo(t)
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt := New(cfg, &runtimeConfigStore{cfg: &cfg, saved: true}, LogRings{})
	rt.started = true
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
	}}

	uiServer := ui.NewServer(0)
	result := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil)
	if !result.LiveApplied {
		t.Fatal("retry did not report live apply after rebuilding missing memory services")
	}
	t.Cleanup(func() { stopRuntime(t, rt) })
	g := rt.gen
	if g == nil || g.sessionMgr == nil || g.taskRunner == nil || g.assembler == nil {
		var sm, tr, asm any
		if g != nil {
			sm, tr, asm = g.sessionMgr, g.taskRunner, g.assembler
		}
		t.Fatalf("memory/API graph was not rebuilt: session=%T task=%T assembler=%T", sm, tr, asm)
	}
	deps, release := rt.AcquireUISnapshot()
	defer release()
	if deps.MemoryRepoPath != root || deps.SessionStore == nil || deps.TaskRunner == nil {
		t.Fatalf("rebuilt UI deps missing: path=%q session=%T task=%T", deps.MemoryRepoPath, deps.SessionStore, deps.TaskRunner)
	}
}
func TestApplyConfigRetriesMissingAPIServerWithoutConfigChange(t *testing.T) {
	root := initRuntimeProjectRepo(t)
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug
	cfg.API.Enabled = false
	cfg.API.Port = freeTCPPort(t)
	store := &runtimeConfigStore{cfg: &cfg, saved: true}

	rt := New(cfg, store, LogRings{})
	rt.started = true
	t.Cleanup(func() { stopRuntime(t, rt) })
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
	}}
	rt.reqQueue = queue.New(cfg.Queue.MaxDepth, rt.newInferenceClient())
	uiServer := ui.NewServer(0)
	if ok := rt.startMemoryAndAPI(context.Background(), uiServer, nil, &rt.cfg); !ok {
		t.Fatal("initial memory service setup failed")
	}
	if rt.apiServer != nil {
		t.Fatal("api server started while API was disabled")
	}

	loaded := cfg
	loaded.API.Enabled = true
	rt.cfg.API.Enabled = true
	store.cfg = &loaded

	result := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil)
	if !result.LiveApplied {
		t.Fatal("retry did not report live apply after starting missing API server")
	}
	if rt.apiServer == nil {
		t.Fatal("API server was not retried when enabled but absent")
	}
}
func TestApplyConfigEndpointChangeRebuildsMemoryServices(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{
			name: "model port",
			mutate: func(c *config.Config) {
				c.Model.Port = 19081
			},
		},
		{
			name: "embedder port",
			mutate: func(c *config.Config) {
				c.Embedder.Port = 19082
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := initRuntimeProjectRepo(t)
			cfg := config.Defaults()
			seedRequiredConfigFiles(t, &cfg)
			cfg.Project.ActiveProjectSlug = project.GlobalSlug
			loaded := cfg
			tc.mutate(&loaded)

			rt := New(cfg, &runtimeConfigStore{cfg: &loaded, saved: true}, LogRings{})
			t.Cleanup(func() { stopRuntime(t, rt) })
			rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
				project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
			}}
			rt.llamaMgr = proc.NewManager(proc.ManagerConfig{Name: "llama-server"})
			rt.embedMgr = proc.NewManager(proc.ManagerConfig{Name: "embedder"})
			rt.inferClient = rt.newInferenceClient()
			rt.reqQueue = queue.New(cfg.Queue.MaxDepth, rt.inferClient)

			// Start a healthy old generation first so the endpoint
			// change predicate (not memoryAPIUnavailable) triggers
			// the rebuild.
			uiServer := ui.NewServer(0)
			if !rt.startMemoryAndAPI(context.Background(), uiServer, nil, &cfg) {
				t.Fatal("initial memory startup failed")
			}
			rt.started = true
			oldGen := rt.gen
			if oldGen == nil || oldGen.sessionMgr == nil {
				t.Fatal("old session manager absent after startup")
			}

			result := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil)
			if !result.LiveApplied {
				t.Fatal("endpoint-only reload did not report a live apply")
			}
			if rt.gen == nil || rt.gen.sessionMgr == nil {
				t.Fatal("endpoint-only reload did not rebuild the session manager")
			}
			if rt.gen.sessionMgr == oldGen.sessionMgr {
				t.Fatal("session manager was not replaced by endpoint change")
			}
			deps, release := rt.AcquireUISnapshot()
			defer release()
			if deps.MemoryRepoPath != root || deps.SessionStore == nil || deps.RetrievalScorer == nil || deps.IndexRebuilder == nil {
				t.Fatalf("endpoint-only reload did not publish rebuilt UI deps: path=%q session=%T scorer=%T rebuilder=%T", deps.MemoryRepoPath, deps.SessionStore, deps.RetrievalScorer, deps.IndexRebuilder)
			}
		})
	}
}

func TestApplyConfigReloadCancelsTaskAndFlushesSession(t *testing.T) {
	root := initRuntimeProjectRepo(t)
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Agent.Active = "coder"
	cfg.Project.ActiveProjectSlug = project.GlobalSlug
	loaded := cfg
	loaded.Prompt.MemoryTokenBudget++

	client := &blockingThenSummaryClient{
		taskToken: inference.Token{Content: "partial answer"},
		summary:   "saved partial task",
	}
	rt := New(cfg, &runtimeConfigStore{cfg: &loaded, saved: true}, LogRings{})
	t.Cleanup(func() { stopRuntime(t, rt) })
	rt.started = true
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
	}}
	rt.inferClient = client
	rt.reqQueue = startRuntimeTestQueue(t, client)

	gitRepo, mgr, _ := newSessionManagerForTest(t, rt, root, client)
	defer func() { _ = gitRepo.Close() }()
	rt.gen.sessionMgr = mgr
	rt.gen.taskRunner = &taskRunnerAdapter{
		rt:         rt,
		registry:   tools.NewRegistry(),
		q:          rt.reqQueue,
		sessionMgr: mgr,
	}

	sessionID, evch, err := rt.gen.taskRunner.RunTask(context.Background(), "coder", "", []ui.ChatMessage{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	select {
	case ev, ok := <-evch:
		if !ok {
			t.Fatal("task event channel closed before text")
		}
		if ev.Type != agentloop.EvtText || ev.Content != "partial answer" {
			t.Fatalf("first task event = %+v, want partial text", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for partial task text")
	}

	result := rt.ApplyConfig(context.Background(), ui.NewServer(0), NewEventChannel(), nil)
	if !result.LiveApplied {
		t.Fatal("reload did not report live apply")
	}
	for range evch {
	}

	snap := mgr.Snapshot(sessionID)
	if snap == nil {
		t.Fatal("old session snapshot missing after reload")
	}
	if !conversationContains(snap.Conversation, "assistant", "partial answer") {
		t.Fatalf("partial assistant text was not recorded before reload flush: %+v", snap.Conversation)
	}
	// The flushed session must be visible to the runtime's current manager.
	// The pre-reload manager's generation-owned reader is closed when the
	// rebuild retires its generation, so records are read through the manager
	// that owns the live reader.
	current := rt.gen.sessionMgr
	if current == nil {
		t.Fatal("session manager absent after reload")
	}
	records, err := current.Records("coder")
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(records) == 0 || records[0].ID != sessionID {
		t.Fatalf("flushed records = %+v, want saved session %s", records, sessionID)
	}
}
func TestStartMemoryAndAPIInvalidRepoDoesNotBindAPI(t *testing.T) {
	port := freeTCPPort(t)
	cfg := config.Defaults()
	cfg.API.Enabled = true
	cfg.API.Port = port

	rt := New(cfg, nil, LogRings{})
	t.Cleanup(func() { stopRuntime(t, rt) })
	rt.reqQueue = queue.New(1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt.startMemoryAndAPI(ctx, ui.NewServer(0), nil, &rt.cfg)

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
	ad := &uiAgentRegistryAdapter{reg: reg, globalMem: mem, activeMem: mem, slug: "global"}

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
	ad := &uiAgentRegistryAdapter{reg: reg, globalMem: mem, activeMem: mem, slug: "global"}

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
	ad := &uiAgentRegistryAdapter{reg: reg, globalMem: global, activeMem: active, slug: "dt"}

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
func TestUIAgentRegistryAdapterWritesProjectScopedPersonaAndRules(t *testing.T) {
	global := newMemoryRepo(t, map[string]string{
		"agents/coder/persona.md": "global persona",
		"agents/coder/rules.md":   "global rules",
	})
	active := newMemoryRepo(t, map[string]string{})
	reg := agent.NewDiskRegistry(global, func() string { return "coder" }, func(string) error { return nil })
	ad := &uiAgentRegistryAdapter{reg: reg, globalMem: global, activeMem: active, slug: "dt"}

	if err := ad.WritePersona("coder", []byte("project persona")); err != nil {
		t.Fatalf("WritePersona: %v", err)
	}
	if err := ad.WriteRules("coder", []byte("project rules")); err != nil {
		t.Fatalf("WriteRules: %v", err)
	}

	projectPersona, err := active.Read("agents/coder/persona.md")
	if err != nil {
		t.Fatalf("read project persona: %v", err)
	}
	if string(projectPersona) != "project persona" {
		t.Fatalf("project persona = %q", string(projectPersona))
	}
	projectRules, err := active.Read("agents/coder/rules.md")
	if err != nil {
		t.Fatalf("read project rules: %v", err)
	}
	if string(projectRules) != "project rules" {
		t.Fatalf("project rules = %q", string(projectRules))
	}
	globalPersona, err := global.Read("agents/coder/persona.md")
	if err != nil {
		t.Fatalf("read global persona: %v", err)
	}
	if string(globalPersona) != "global persona" {
		t.Fatalf("global persona changed to %q", string(globalPersona))
	}
	globalRules, err := global.Read("agents/coder/rules.md")
	if err != nil {
		t.Fatalf("read global rules: %v", err)
	}
	if string(globalRules) != "global rules" {
		t.Fatalf("global rules changed to %q", string(globalRules))
	}

	info, err := ad.Get("coder")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Origin != "extends-global" || info.Persona != "project persona" || info.Rules != "project rules" {
		t.Fatalf("project override not reflected in UI info: %+v", info)
	}
}

func TestUIAgentRegistryAdapterDeletesOnlyProjectAgentInProjectScope(t *testing.T) {
	global := newMemoryRepo(t, map[string]string{
		"agents/coder/persona.md": "global persona",
	})
	active := newMemoryRepo(t, map[string]string{
		"agents/coder/persona.md": "project persona",
		"agents/coder/notes.md":   "project notes",
	})
	currentActive := "coder"
	reg := agent.NewDiskRegistry(global, func() string { return currentActive }, func(name string) error { currentActive = name; return nil })
	ad := &uiAgentRegistryAdapter{
		reg:       reg,
		globalMem: global,
		activeMem: active,
		slug:      "dt",
		setActive: func(name string) error { currentActive = name; return nil },
	}

	if err := ad.Delete("coder"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if currentActive != "" {
		t.Fatalf("active = %q, want cleared", currentActive)
	}
	globalPersona, err := global.Read("agents/coder/persona.md")
	if err != nil {
		t.Fatalf("global persona missing after project delete: %v", err)
	}
	if string(globalPersona) != "global persona" {
		t.Fatalf("global persona = %q", string(globalPersona))
	}
	info, err := ad.Get("coder")
	if err != nil {
		t.Fatalf("Get after project delete: %v", err)
	}
	if info.Origin != "global" || info.Persona != "global persona" {
		t.Fatalf("expected fallback to global after project delete, got %+v", info)
	}
}

func TestUIAgentRegistryAdapterSetsProjectOnlyAgentActive(t *testing.T) {
	global := newMemoryRepo(t, map[string]string{})
	active := newMemoryRepo(t, map[string]string{
		"agents/local/persona.md": "local persona",
	})
	currentActive := ""
	reg := agent.NewDiskRegistry(global, func() string { return currentActive }, func(name string) error { currentActive = name; return nil })
	ad := &uiAgentRegistryAdapter{
		reg:       reg,
		globalMem: global,
		activeMem: active,
		slug:      "dt",
		setActive: func(name string) error { currentActive = name; return nil },
	}

	if err := ad.SetActive("local"); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if currentActive != "local" {
		t.Fatalf("active = %q, want local", currentActive)
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
		ToolID:   "edit",
		Decision: approvals.Allowed,
		Source:   "session: always allowed",
	})

	if got, _ := first.Evaluate("edit", ""); got != approvals.Allowed {
		t.Fatalf("first evaluator decision = %v, want Allowed", got)
	}
	if got, _ := second.Evaluate("edit", ""); got != approvals.Ask {
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
	t.Cleanup(func() { stopRuntime(t, rt) })
	// The first call (the task turn) blocks on cancellation after one token;
	// later calls (the shutdown session flush) complete with a summary and
	// Done, so a retained session can be saved by a later shutdown attempt.
	rt.inferClient = &blockingThenSummaryClient{
		taskToken: inference.Token{Content: "partial answer"},
		summary:   "cancelled partial task",
	}

	gitRepo, mgr, _ := newSessionManagerForTest(t, rt, root, rt.ensureInferenceClient())
	defer func() { _ = gitRepo.Close() }()

	ad := &taskRunnerAdapter{rt: rt, registry: tools.NewRegistry(), q: startRuntimeTestQueue(t, rt.ensureInferenceClient()), sessionMgr: mgr}
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
	t.Cleanup(func() { stopRuntime(t, rt) })
	gitRepo, mgr, _ := newSessionManagerForTest(t, rt, root, rt.ensureInferenceClient())
	defer func() { _ = gitRepo.Close() }()
	if mgr == nil {
		t.Fatal("buildSessionManager returned nil")
	}
	s := mgr.Start("coder")

	recordTaskEvents(mgr, s.ID, []agentloop.Event{
		{
			Type:           agentloop.EvtApprovalNeeded,
			ApprovalID:     "approval-1",
			ToolID:         "edit",
			ToolArgs:       `{"path":"x"}`,
			ApprovalReason: "builtin: edits require approval",
		},
		{
			Type:             agentloop.EvtApproval,
			ApprovalID:       "approval-1",
			ToolID:           "edit",
			ApprovalReason:   "builtin: edits require approval",
			ApprovalDecision: approvals.Allowed.String(),
			ApprovalScope:    approvals.ApprovalScopeAlways,
		},
	})

	snap := mgr.Snapshot(s.ID)
	if snap == nil {
		t.Fatal("session snapshot missing")
	}
	var approvalMessages []string
	for _, msg := range snap.Conversation {
		if msg.Role == "system" && msg.Name == "approval" {
			approvalMessages = append(approvalMessages, msg.Content)
		}
	}
	if len(approvalMessages) != 2 {
		t.Fatalf("approval audit messages = %d, want 2; conversation=%+v", len(approvalMessages), snap.Conversation)
	}
	for _, want := range []string{"[approval_needed #1]", "id=approval-1", "tool=edit", "reason=\"builtin: edits require approval\"", `args={"path":"x"}`} {
		if !strings.Contains(approvalMessages[0], want) {
			t.Fatalf("approval-needed audit missing %q: %#v", want, approvalMessages)
		}
	}
	for _, want := range []string{"[approval #1]", "id=approval-1", "tool=edit", "decision=" + approvals.Allowed.String(), "scope=" + approvals.ApprovalScopeAlways, "reason=\"builtin: edits require approval\""} {
		if !strings.Contains(approvalMessages[1], want) {
			t.Fatalf("approval result audit missing %q: %#v", want, approvalMessages)
		}
	}
	if strings.Contains(approvalMessages[1], "#2") {
		t.Fatalf("approval result consumed a new number: %#v", approvalMessages)
	}
}

func TestTaskRunnerAppendsDistinctFollowUpOnResume(t *testing.T) {
	root := initRuntimeProjectRepo(t)
	cfg := config.Defaults()
	cfg.Project.ActiveProjectSlug = "global"
	rt := New(cfg, nil, LogRings{})
	t.Cleanup(func() { stopRuntime(t, rt) })
	rt.inferClient = &capturingInferenceClient{tokens: []inference.Token{{Content: "ok"}, {Done: true}}}

	gitRepo, mgr, _ := newSessionManagerForTest(t, rt, root, rt.ensureInferenceClient())
	defer func() { _ = gitRepo.Close() }()

	s := mgr.Start("coder")
	if err := mgr.Append(s.ID, inference.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatalf("seed Append: %v", err)
	}

	ad := &taskRunnerAdapter{rt: rt, registry: tools.NewRegistry(), q: startRuntimeTestQueue(t, rt.ensureInferenceClient()), sessionMgr: mgr}
	_, evch, err := ad.RunTask(context.Background(), "coder", s.ID, []ui.ChatMessage{{Role: "user", Content: "hello"}, {Role: "user", Content: "follow-up"}})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	for range evch {
	}

	snap := mgr.Snapshot(s.ID)
	if snap == nil {
		t.Fatal("session snapshot missing")
	}
	userTurns := map[string]int{}
	for _, msg := range snap.Conversation {
		if msg.Role == "user" {
			userTurns[msg.Content]++
		}
	}
	if userTurns["hello"] != 1 || userTurns["follow-up"] != 1 {
		t.Fatalf("user turns = %#v, want one hello and one follow-up; conversation=%+v", userTurns, snap.Conversation)
	}
}
func TestTaskRunnerWiresHTTPClientIntoToolContext(t *testing.T) {
	cfg := config.Defaults()
	cfg.Agent.Active = "coder"
	cfg.Project.ActiveProjectSlug = project.GlobalSlug
	cfg.Loop.WebSearchEnabled = true

	client := &sequenceInferenceClient{sequences: [][]inference.Token{
		{
			{ToolCallDelta: &inference.ToolCallDelta{
				Index:     0,
				ID:        "call-1",
				Name:      "web_search",
				Arguments: `{"query":"local harness"}`,
			}},
			{Done: true},
		},
		{
			{Content: "done"},
			{Done: true},
		},
	}}

	rt := New(cfg, nil, LogRings{})
	t.Cleanup(func() { stopRuntime(t, rt) })
	rt.inferClient = client

	probe := &httpClientProbeTool{}
	registry := tools.NewRegistry()
	if err := registry.Register(probe); err != nil {
		t.Fatalf("Register probe tool: %v", err)
	}
	ad := &taskRunnerAdapter{rt: rt, registry: registry, q: startRuntimeTestQueue(t, rt.ensureInferenceClient()), loopCfg: cfg.Loop}

	_, evch, err := ad.RunTask(context.Background(), "coder", "", []ui.ChatMessage{{Role: "user", Content: "search"}})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	for range evch {
	}

	probe.mu.Lock()
	defer probe.mu.Unlock()
	if !probe.called {
		t.Fatal("probe tool was not called")
	}
	if !probe.sawHTTPClient {
		t.Fatal("tool context HTTPClient was nil")
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
				Name:      "read",
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
	t.Cleanup(func() { stopRuntime(t, rt) })
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
	ad := &taskRunnerAdapter{rt: rt, registry: registry, q: startRuntimeTestQueue(t, rt.ensureInferenceClient()), loopCfg: cfg.Loop}

	_, evch, err := ad.RunTask(context.Background(), "coder", "", []ui.ChatMessage{{Role: "user", Content: "read the file"}})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	sawRead := false
	for ev := range evch {
		if ev.Type != agentloop.EvtToolResult || ev.ToolID != "read" {
			continue
		}
		sawRead = true
		if !strings.Contains(ev.ToolError, "sandbox") {
			t.Fatalf("read ToolError = %q, want sandbox error", ev.ToolError)
		}
		if strings.Contains(ev.ToolResult, "secret memory contents") {
			t.Fatalf("read returned memory repo content despite no configured project directories: %q", ev.ToolResult)
		}
	}
	if !sawRead {
		t.Fatal("did not observe read tool result")
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
	mem, err := memory.NewDirReader(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close() })
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
	t.Cleanup(func() { stopRuntime(t, rt) })
	asm := prompt.NewProjectDiskAssembler(mem, mem, reg, cfg.Prompt).WithProjectSlug("global")
	rt.gen = &generation{globalMem: mem, activeMem: mem, agentReg: reg, assembler: asm}
	rt.gen.acquire()
	rt.inferClient = failingInferenceClient{err: fmt.Errorf("direct inference path used")}

	ad := &taskRunnerAdapter{
		rt:       rt,
		asm:      &staticAssembler{asm: asm, active: "coder"},
		registry: tools.NewRegistry(),
		q:        q,
		slug:     "global",
		loopCfg:  cfg.Loop,
		mem:      mem,
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
	t.Cleanup(func() { stopRuntime(t, rt) })
	dr, err := memory.NewDirReader(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dr.Close() })
	rt.gen = &generation{globalMem: dr, activeMem: dr}
	rt.gen.acquire()

	gitRepo, mgr, adapter, err := rt.buildSessionManagerWithClients(nil, projectRepoRoots{
		globalRoot: root,
		activeRoot: root,
		activeSlug: project.GlobalSlug,
	}, rt.ensureInferenceClient(), nil, nil, dr, project.GlobalSlug)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gitRepo.Close() }()
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
	seedRuntimeProjectRepoAt(t, root)
	return root
}

// seedRuntimeProjectRepoAt initializes a project memory repo with its
// scaffold at an explicit path (used when the repo must live under a
// particular parent so a junction can select between two of them).
func seedRuntimeProjectRepoAt(t *testing.T, root string) {
	t.Helper()
	repo, err := gitw.Init(root)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	defer func() { _ = repo.Close() }()
	if err := memory.CreateMissing(root, memory.ExpectedProjectRepoLayout(true)); err != nil {
		t.Fatalf("scaffold project repo: %v", err)
	}
	if _, err := repo.Commit(gitw.BuildMessage(map[string]string{"type": "scaffold"}, "initialize project memory repo"), memory.ProjectRepoScaffoldFiles(true)); err != nil {
		t.Fatalf("commit scaffold: %v", err)
	}
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
func startRuntimeTestQueue(t *testing.T, client inference.Client) *queue.Queue {
	t.Helper()
	q := queue.New(4, client)
	ctx, cancel := context.WithCancel(context.Background())
	if err := q.Start(ctx); err != nil {
		cancel()
		t.Fatalf("queue Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		q.Stop()
	})
	return q
}

type httpClientProbeTool struct {
	mu            sync.Mutex
	called        bool
	sawHTTPClient bool
}

func (t *httpClientProbeTool) ID() string { return "web_search" }

func (t *httpClientProbeTool) Description() string { return "probe web search tool context" }

func (t *httpClientProbeTool) Schema() map[string]any {
	return map[string]any{"type": "object"}
}

func (t *httpClientProbeTool) Execute(_ context.Context, c tools.CallInfo, _ map[string]any) tools.Result {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.called = true
	t.sawHTTPClient = c.HTTPClient != nil
	return tools.Result{Content: "ok"}
}

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

func conversationContains(msgs []inference.Message, role, content string) bool {
	for _, msg := range msgs {
		if msg.Role == role && msg.Content == content {
			return true
		}
	}
	return false
}

type blockingThenSummaryClient struct {
	mu        sync.Mutex
	calls     int
	taskToken inference.Token
	summary   string
}

func (c *blockingThenSummaryClient) Complete(ctx context.Context, _ inference.CompletionRequest) (<-chan inference.Token, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	taskToken := c.taskToken
	summary := c.summary
	c.mu.Unlock()

	ch := make(chan inference.Token, 2)
	if call == 1 {
		go func() {
			defer close(ch)
			ch <- taskToken
			<-ctx.Done()
		}()
		return ch, nil
	}
	ch <- inference.Token{Content: summary}
	ch <- inference.Token{Done: true}
	close(ch)
	return ch, nil
}

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

type runtimeConfigStore struct {
	cfg   *config.Config
	saved bool
	// onSave, when set, runs after each successful Save. Tests use it as a
	// seam to observe store writes from concurrent operations.
	onSave func()
}

func (s *runtimeConfigStore) Load() (*config.Config, bool, error) {
	if s.cfg == nil {
		cfg := config.Defaults()
		return &cfg, s.saved, nil
	}
	cfg := *s.cfg
	return &cfg, s.saved, nil
}

func (s *runtimeConfigStore) Save(cfg *config.Config) error {
	copied := *cfg
	s.cfg = &copied
	s.saved = true
	if s.onSave != nil {
		s.onSave()
	}
	return nil
}

func seedRequiredConfigFiles(t *testing.T, cfg *config.Config) {
	t.Helper()
	dir := t.TempDir()
	paths := []*string{&cfg.Model.Binary, &cfg.Model.ModelPath, &cfg.Embedder.Binary, &cfg.Embedder.ModelPath}
	for i, target := range paths {
		path := filepath.Join(dir, fmt.Sprintf("required-%d", i))
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		*target = path
	}
}

type runtimeProjectStoreStub struct {
	projects map[string]project.Project
	dirs     map[string][]project.Directory
	listErr  error // when set, List fails — exercises fail-closed callers
	// updateCalls counts successful Update mutations. Tests use it to prove an
	// edit was refused before any metadata was written.
	updateCalls int
}

func (s *runtimeProjectStoreStub) List(bool) ([]project.Project, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
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

func (s *runtimeProjectStoreStub) Update(input project.UpdateInput) (project.Project, error) {
	p, ok := s.projects[input.Slug]
	if !ok {
		return project.Project{}, project.ErrNotFound
	}
	p.DisplayName = input.DisplayName
	p.MemoryRepoPath = input.MemoryRepoPath
	p.ModelBinary = input.ModelBinary
	p.ModelPath = input.ModelPath
	p.ModelCtxSize = input.ModelCtxSize
	p.ModelGPULayers = input.ModelGPULayers
	p.ModelNParallel = input.ModelNParallel
	s.projects[input.Slug] = p
	s.updateCalls++
	return p, nil
}

func (s *runtimeProjectStoreStub) SetHidden(string, bool) error { return nil }

func (s *runtimeProjectStoreStub) ListDirectories(slug string) ([]project.Directory, error) {
	return append([]project.Directory(nil), s.dirs[slug]...), nil
}

type failingInferenceClient struct {
	err error
}

func (f failingInferenceClient) Complete(context.Context, inference.CompletionRequest) (<-chan inference.Token, error) {
	return nil, f.err
}

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
	dr, err := memory.NewDirReader(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dr.Close() })
	return dr
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

func TestReloadReleasesPreviousHandles(t *testing.T) {
	oldRoot := initRuntimeProjectRepo(t)
	newRoot := initRuntimeProjectRepo(t)

	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt := New(cfg, &runtimeConfigStore{cfg: &cfg, saved: true}, LogRings{})
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: oldRoot},
	}}

	uiServer := ui.NewServer(0)
	rt.Start(context.Background(), uiServer, NewEventChannel(), nil)
	t.Cleanup(func() { stopRuntime(t, rt) })

	// Now reload to newRoot. After success, oldRoot should be removable.
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: newRoot},
	}}
	loaded := cfg
	loaded.Prompt.MemoryTokenBudget++
	store := &runtimeConfigStore{cfg: &loaded, saved: true}
	rt.cfgStore = store

	result := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil)
	if !result.LiveApplied {
		t.Fatal("reload to newRoot did not report live apply")
	}

	if err := os.RemoveAll(oldRoot); err != nil {
		t.Fatalf("old root was not released after successful reload: %v", err)
	}
}

func TestCandidateFailureReleasesAllCandidateHandles(t *testing.T) {
	root := initRuntimeProjectRepo(t)

	// Place a corrupt manifest inside index/_episodes so NewEpisodeIndex
	// fails after DirReaders are open. This exercises handle cleanup on
	// the candidate path.
	manifestDir := filepath.Join(root, "index", "_episodes")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.json"), []byte("{not json}"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug
	loaded := cfg
	loaded.Prompt.MemoryTokenBudget++

	rt := New(cfg, &runtimeConfigStore{cfg: &loaded, saved: true}, LogRings{})
	rt.started = true
	t.Cleanup(func() { stopRuntime(t, rt) })
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
	}}

	uiServer := ui.NewServer(0)
	result := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil)
	if result.LiveApplied {
		t.Fatal("reload with blocked episode index should not report live apply")
	}

	// The original root should still be accessible after candidate failure.
	if _, err := os.ReadFile(filepath.Join(root, "rules.md")); err != nil {
		t.Fatalf("original root files not accessible after failed reload: %v", err)
	}
}

// TestCandidateIdentityMismatchFailsClosed verifies that candidate
// construction fails closed when the git repository and the memory/project
// reader resolve to different physical repositories, and that the failure
// releases the candidate's handles and preserves the installed generation
// usable.
//
// The installed generation is constructed on a stable direct path, so its
// readers stay readable. The failed candidate is staged through a separate
// alias: a junction on an intermediate component is repointed between the
// memory open and the git open inside buildCandidate, so the candidate's
// memory reader pins one physical repo while its git handle pins another.
// If the two boundaries were not compared, the candidate would run git
// commits and index publication under two different coordinators.
func TestCandidateIdentityMismatchFailsClosed(t *testing.T) {
	stableParent := t.TempDir()
	stable := filepath.Join(stableParent, "repo")
	seedRuntimeProjectRepoAt(t, stable)
	parentB := t.TempDir()
	seedRuntimeProjectRepoAt(t, filepath.Join(parentB, "repo"))
	base := t.TempDir()
	junction := filepath.Join(base, "active")
	linkDir(t, stableParent, junction)
	aliasRoot := filepath.Join(junction, "repo")

	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug
	loaded := cfg
	loaded.Prompt.MemoryTokenBudget++

	rt := New(cfg, &runtimeConfigStore{cfg: &loaded, saved: true}, LogRings{})
	rt.started = true
	t.Cleanup(func() { stopRuntime(t, rt) })
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: stable},
	}}

	// First apply succeeds on a stable direct path: git and memory both pin
	// stable, so the installed generation's readers are not reachable through
	// any alias that a later candidate repoints.
	uiServer := ui.NewServer(0)
	if !rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil).LiveApplied {
		t.Fatal("first apply with matching git/memory identity did not apply live")
	}

	// Reload through the alias: repoint the intermediate junction between the
	// memory open and the git open so the candidate's git repository and
	// memory reader identify different physical repositories.
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: aliasRoot},
	}}
	rt.beforeGitOpen = func() {
		if err := os.RemoveAll(junction); err != nil {
			t.Fatalf("remove active junction: %v", err)
		}
		linkDir(t, parentB, junction)
	}
	result := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil)
	if result.LiveApplied {
		t.Fatal("reload with mismatched git/memory identity reported live apply")
	}

	// The installed generation must remain usable: read through its reader,
	// not through the repointed alias. A read that succeeds proves the
	// reader's retained anchor is still open and still points at the stable
	// directory the generation was constructed on.
	if _, err := rt.gen.activeMem.Read("rules.md"); err != nil {
		t.Fatalf("installed generation reader failed after rejected reload: %v", err)
	}
}

// TestCandidateIdentityMismatchSameNameReplacementFailsClosed is the
// discriminator that proves the runtime comparison is handle-bound, not
// pathid-based. A directory is replaced at the same pathname between the
// memory open and the git open: the two physical repositories canonicalize to
// the same pathid key, so an Identity().Equal comparison would accept the
// candidate, while the retained-boundary SameRepo comparison must reject it.
func TestCandidateIdentityMismatchSameNameReplacementFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows: the retained memory reader handle blocks replacing the directory at the same pathname; the handle-block is the fail-closed outcome")
	}
	base := t.TempDir()
	activeRoot := filepath.Join(base, "repo")
	seedRuntimeProjectRepoAt(t, activeRoot)
	origID, err := pathid.Resolve(activeRoot)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt := New(cfg, &runtimeConfigStore{cfg: &cfg, saved: true}, LogRings{})
	rt.started = true
	t.Cleanup(func() { stopRuntime(t, rt) })
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: activeRoot},
	}}

	// Between the memory open and the git open, rename the repository aside
	// and create a different physical repository at the same pathname.
	rt.beforeGitOpen = func() {
		if err := os.Rename(activeRoot, activeRoot+".aside"); err != nil {
			t.Fatalf("rename repo aside: %v", err)
		}
		seedRuntimeProjectRepoAt(t, activeRoot)
	}
	result := rt.ApplyConfig(context.Background(), ui.NewServer(0), NewEventChannel(), nil)
	if result.LiveApplied {
		t.Fatal("candidate with equal pathid keys but different retained handles was accepted; the comparison must be handle-bound")
	}

	// Precondition: the same-name replacement reuses the pathid key, so the
	// rejection cannot be explained by a pathid-based comparison.
	replacedID, err := pathid.Resolve(activeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !origID.Equal(replacedID) {
		t.Fatal("same-name replacement must reuse the pathid key, or this test no longer discriminates")
	}
}

func TestFailedReloadPreservesReadableGeneration(t *testing.T) {
	root := initRuntimeProjectRepo(t)
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	mem, err := memory.NewDirReader(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close() })

	loaded := cfg
	loaded.Project.ActiveProjectSlug = "missing"

	rt := New(cfg, &runtimeConfigStore{cfg: &loaded, saved: true}, LogRings{})
	rt.started = true
	t.Cleanup(func() { stopRuntime(t, rt) })
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
	}}

	if err := os.WriteFile(filepath.Join(root, "known.txt"), []byte("restored"), 0o644); err != nil {
		t.Fatal(err)
	}

	uiServer := ui.NewServer(0)
	// The existing generation owns the published snapshot and the readable
	// reader; a failed reload must leave both untouched.
	rt.gen = &generation{globalMem: mem, activeMem: mem, uiSnap: ui.ServiceDeps{MemoryRepoPath: root, MemoryStore: mem}}
	rt.gen.acquire()

	result := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil)
	if result.LiveApplied {
		t.Fatal("failed memory/API reload should not report live apply")
	}

	got, gerr := mem.Read("known.txt")
	if gerr != nil {
		t.Fatalf("restored generation Read failed: %v", gerr)
	}
	if string(got) != "restored" {
		t.Fatalf("restored generation Read = %q, want restored", string(got))
	}
}

// TestGenerationReaderOwnershipRetiredOnShutdown verifies the generation-owned
// active reader — which the session manager now reads and writes directly —
// is closed when Shutdown retires the generation.
func TestGenerationReaderOwnershipRetiredOnShutdown(t *testing.T) {
	root := initRuntimeProjectRepo(t)
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt := New(cfg, nil, LogRings{})
	rt.started = true
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
	}}

	// A live generation owns the active reader; the session manager reads and
	// writes it. Shutdown must close it when the last lease is released.
	reader, err := memory.NewDirReader(root)
	if err != nil {
		t.Fatal(err)
	}
	rt.gen = &generation{globalMem: reader, activeMem: reader}
	rt.gen.acquire()

	stopRuntime(t, rt)

	if _, err := reader.Read("rules.md"); err == nil {
		t.Error("generation reader Read should fail after Shutdown closed it")
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	root := initRuntimeProjectRepo(t)
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt := New(cfg, nil, LogRings{})
	rt.started = true
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
	}}

	gitRepo, _, _ := newSessionManagerForTest(t, rt, root, rt.ensureInferenceClient())
	defer func() { _ = gitRepo.Close() }()

	stopRuntime(t, rt)
	if rt.gen != nil {
		t.Error("session manager should be nil after Shutdown")
	}
	stopRuntime(t, rt)
}

func TestGenLeaseSurvivesReload(t *testing.T) {
	root := initRuntimeProjectRepo(t)
	// Create agent persona so assembly succeeds.
	if err := os.MkdirAll(filepath.Join(root, "agents", "coder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "coder", "persona.md"), []byte("coder persona"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug
	cfg.API.Enabled = true
	cfg.API.Port = freeTCPPort(t)

	rt := New(cfg, nil, LogRings{})
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
	}}
	rt.reqQueue = queue.New(1, nil)
	rt.started = true

	uiServer := ui.NewServer(0)
	if !rt.startMemoryAndAPI(context.Background(), uiServer, nil, &rt.cfg) {
		t.Fatal("initial start failed")
	}
	t.Cleanup(func() { stopRuntime(t, rt) })

	asm, rec, _, release := rt.AcquireRequestGeneration()

	// Reload while holding the lease on the original generation.
	loaded := cfg
	loaded.Prompt.MemoryTokenBudget++
	store := &runtimeConfigStore{cfg: &loaded, saved: true}
	rt.cfgStore = store
	result := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil)
	if !result.LiveApplied {
		release()
		t.Fatal("reload failed while holding lease")
	}

	// Reload a second time.
	loaded.Prompt.MemoryTokenBudget++
	store.cfg = &loaded
	result = rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil)
	if !result.LiveApplied {
		release()
		t.Fatal("second reload failed while holding lease")
	}

	// The captured assembler and recorder must still be usable —
	// their generation's readers are pinned by the lease.
	msgs, err := asm.Assemble(context.Background(), "coder", []inference.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		release()
		t.Fatalf("assemble after reloads: %v", err)
	}
	if len(msgs) == 0 {
		release()
		t.Fatal("assemble returned no messages")
	}

	sess := rec.Start("coder")
	if sess.ID == "" {
		release()
		t.Fatal("session Start failed")
	}
	rec.End(sess.ID)
	release()
}

func TestGenLeaseKeepsRecordingInOriginalProject(t *testing.T) {
	modelPort, shutdownModel := startFakeModelServer(t, "test summary")
	defer shutdownModel()

	oldRoot := initRuntimeProjectRepo(t)
	newRoot := initRuntimeProjectRepo(t)
	if err := os.MkdirAll(filepath.Join(oldRoot, "agents", "coder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "agents", "coder", "persona.md"), []byte("coder persona"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug
	cfg.Model.Port = modelPort
	cfg.Embedder.Port = freeTCPPort(t)

	rt := New(cfg, &runtimeConfigStore{cfg: &cfg, saved: true}, LogRings{})
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: oldRoot},
	}}
	rt.reqQueue = queue.New(1, nil)

	uiServer := ui.NewServer(0)
	rt.Start(context.Background(), uiServer, NewEventChannel(), nil)
	t.Cleanup(func() { stopRuntime(t, rt) })

	asm, rec, _, release := rt.AcquireRequestGeneration()
	defer release()

	// Reload to newRoot while holding the lease on the original generation.
	loaded := cfg
	loaded.Prompt.MemoryTokenBudget++
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: newRoot},
	}}
	store := &runtimeConfigStore{cfg: &loaded, saved: true}
	rt.cfgStore = store
	if !rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil).LiveApplied {
		t.Fatal("reload failed")
	}

	// Assemble and record through the captured (oldRoot) generation.
	_, err := asm.Assemble(context.Background(), "coder", []inference.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	sess := rec.Start("coder")
	if err := rec.Append(sess.ID, "user", "hello"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := rec.Save(context.Background(), sess.ID); err != nil {
		t.Fatalf("Save: %v", err)
	}
	rec.End(sess.ID)

	// Episode files must exist in oldRoot (captured generation's manager),
	// not in newRoot (generation published by reload).
	ep := filepath.Join("episodes", "coder", sess.ID+".md")
	js := filepath.Join("episodes", "coder", sess.ID+".json")
	if _, err := os.Stat(filepath.Join(oldRoot, filepath.FromSlash(ep))); err != nil {
		t.Fatalf("episode not in oldRoot %s: %v", ep, err)
	}
	if _, err := os.Stat(filepath.Join(oldRoot, filepath.FromSlash(js))); err != nil {
		t.Fatalf("sidecar not in oldRoot %s: %v", js, err)
	}
	if _, err := os.Stat(filepath.Join(newRoot, filepath.FromSlash(ep))); !os.IsNotExist(err) {
		t.Fatalf("episode incorrectly landed in newRoot: %s", ep)
	}
}

func TestGenLeasePinsOldRootUntilReleased(t *testing.T) {
	oldRoot := initRuntimeProjectRepo(t)
	newRoot := initRuntimeProjectRepo(t)

	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug
	cfg.API.Enabled = true
	cfg.API.Port = freeTCPPort(t)

	rt := New(cfg, &runtimeConfigStore{cfg: &cfg, saved: true}, LogRings{})
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: oldRoot},
	}}
	rt.reqQueue = queue.New(1, nil)

	uiServer := ui.NewServer(0)
	rt.Start(context.Background(), uiServer, NewEventChannel(), nil)
	t.Cleanup(func() { stopRuntime(t, rt) })

	_, _, _, release := rt.AcquireRequestGeneration()

	// Reload to newRoot while holding the lease.
	loaded := cfg
	loaded.Prompt.MemoryTokenBudget++
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: newRoot},
	}}
	store := &runtimeConfigStore{cfg: &loaded, saved: true}
	rt.cfgStore = store
	if !rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil).LiveApplied {
		release()
		t.Fatal("reload failed")
	}

	// oldRoot should be pinned by the lease. On Windows removing a
	// directory with open handles must fail.
	if err := os.RemoveAll(oldRoot); err == nil {
		if runtime.GOOS == "windows" {
			release()
			t.Fatal("old root was removable despite held lease on Windows")
		}
		// Unix: inode-based handles allow removal even while open.
	} else {
		t.Logf("old root blocked by lease: %v", err)
	}

	release()
	stopRuntime(t, rt)
	// After release + Shutdown, oldRoot must be removable even on Windows.
	if err := os.RemoveAll(oldRoot); err != nil {
		t.Fatalf("old root not removable after release and Shutdown: %v", err)
	}
}

// TestSnapshot_OldReferencesRemainValidAfterReload verifies that a UI
// snapshot acquired before a reload keeps its generation's readers usable
// against the original repository after the reload retires the old
// generation. The old root stays pinned while the snapshot is held (a fatal
// Windows assertion where handle blocking is the discriminator), retires
// after release, and Shutdown does not close resources protected by an
// outstanding snapshot lease. All waiting is barrier-free: the assertions are
// synchronous and deterministic.
func TestSnapshot_OldReferencesRemainValidAfterReload(t *testing.T) {
	oldRoot := initRuntimeProjectRepo(t)
	newRoot := initRuntimeProjectRepo(t)
	if err := os.WriteFile(filepath.Join(oldRoot, "known.txt"), []byte("leased"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt := New(cfg, &runtimeConfigStore{cfg: &cfg, saved: true}, LogRings{})
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: oldRoot},
	}}
	rt.reqQueue = queue.New(1, nil)

	uiServer := ui.NewServer(0)
	rt.Start(context.Background(), uiServer, NewEventChannel(), nil)
	t.Cleanup(func() { stopRuntime(t, rt) })

	// The held-lease phase runs inside a closure so every assertion path
	// releases exactly once via the deferred release.
	func() {
		snap, release := rt.AcquireUISnapshot()
		defer release()

		// Reload to newRoot while holding the snapshot lease on the original
		// generation.
		loaded := cfg
		loaded.Prompt.MemoryTokenBudget++
		rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
			project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: newRoot},
		}}
		store := &runtimeConfigStore{cfg: &loaded, saved: true}
		rt.cfgStore = store
		if !rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil).LiveApplied {
			t.Fatal("reload failed")
		}

		// The held snapshot must still perform a real rooted read against the
		// original repository.
		memStore := snap.MemoryStore
		if memStore == nil {
			t.Fatal("held snapshot has no memory store")
		}
		if b, err := memStore.Read("known.txt"); err != nil || string(b) != "leased" {
			t.Fatalf("held snapshot read = %q, %v", b, err)
		}

		// Shutdown must not close resources protected by an outstanding
		// snapshot lease.
		stopRuntime(t, rt)
		if _, err := memStore.Read("known.txt"); err != nil {
			t.Fatalf("held snapshot read failed after Shutdown: %v", err)
		}

		// Old handles stay pinned while the snapshot is held. The RemoveAll
		// checks run last because a blocked RemoveAll still deletes files
		// under the held directory; the reads above must run first.
		if err := os.RemoveAll(oldRoot); err == nil {
			if runtime.GOOS == "windows" {
				t.Fatal("old root was removable despite held snapshot on Windows")
			}
			// Unix: inode-based handles allow removal even while open.
		} else {
			t.Logf("old root blocked by held snapshot: %v", err)
		}
	}()

	// After the last acquired snapshot is released, the old readers close and
	// the old root must be removable even on Windows.
	if err := os.RemoveAll(oldRoot); err != nil {
		t.Fatalf("old root not removable after snapshot release: %v", err)
	}
}

// TestSnapshot_RetryOnlyStartupWiresProvider verifies that a runtime which
// never passes through Start — first run, invalid initial config, or failed
// path validation — still installs the snapshot provider on the UI server
// when a retry succeeds. Before the fix the provider was installed only by
// Start, so a retry-only ApplyConfig published a real generation but every
// generation-backed UI handler kept receiving an empty snapshot.
func TestSnapshot_RetryOnlyStartupWiresProvider(t *testing.T) {
	root := initRuntimeProjectRepo(t)
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt := New(cfg, &runtimeConfigStore{cfg: &cfg, saved: true}, LogRings{})
	t.Cleanup(func() { stopRuntime(t, rt) })
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
	}}
	rt.reqQueue = queue.New(1, nil)

	// rt.Start is deliberately never called — this is the retry-only path.

	port := freeTCPPort(t)
	uiServer := ui.NewServer(port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := uiServer.Start(ctx); err != nil {
		t.Fatalf("ui server start: %v", err)
	}

	result := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil)
	if !result.LiveApplied {
		t.Fatal("retry-only startup did not report live apply")
	}

	// A generation was published and carries a chat runner.
	deps, release := rt.AcquireUISnapshot()
	if deps.ChatRunner == nil {
		release()
		t.Fatal("retry-only startup did not publish a chat runner")
	}
	release()

	// A real handler request must observe that runner. Before the fix the UI
	// server had no provider, so /chat rendered the unconfigured CTA even
	// though a generation existed.
	var body string
	for i := 0; i < 20; i++ {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/chat", port))
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		b, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr != nil {
			t.Fatalf("read /chat body: %v", rerr)
		}
		body = string(b)
		break
	}
	if body == "" {
		t.Fatal("could not reach the ui server's /chat route")
	}
	if strings.Contains(body, "Chat backend not ready") {
		t.Fatal("retry-only startup left the UI without a snapshot provider: /chat rendered the unconfigured CTA")
	}
}

// requestCapture records every completion request body the fake model server
// receives, so a test can assert which generation's prompt files were used to
// assemble it. first is closed on the first request so tests can wait
// barrier-style for a task to reach the model.
type requestCapture struct {
	mu     sync.Mutex
	bodies []string
	first  chan struct{}
	once   sync.Once
}

func (c *requestCapture) add(body string) {
	c.mu.Lock()
	c.bodies = append(c.bodies, body)
	c.mu.Unlock()
	c.once.Do(func() { close(c.first) })
}

func (c *requestCapture) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...)
}

// startCapturingModelServer serves /v1/chat/completions like a real
// llama-server, recording each request body so the test can prove which
// generation assembled it.
func startCapturingModelServer(t *testing.T, summary string) (int, func(), *requestCapture) {
	t.Helper()
	capture := &requestCapture{first: make(chan struct{})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake model: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capture.add(string(body))
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
	}, capture
}

// TestSnapshot_RunnerReadsAndRecordsInOriginalProject proves that the chat
// and task runners captured in an old UI snapshot assemble their prompts from,
// and record their sessions to, exclusively the project they were published
// for — even after the runtime reloads to a different project. Before the fix
// the snapshot's runners dereferenced Runtime: the chat/task assembler
// reacquired the live generation (so an old snapshot would assemble with the
// new project's rules) and the task runner re-read the live session manager
// (so it would record into the new project).
func TestSnapshot_RunnerReadsAndRecordsInOriginalProject(t *testing.T) {
	modelPort, shutdownModel, capture := startCapturingModelServer(t, "test summary")
	defer shutdownModel()

	oldRoot := initRuntimeProjectRepo(t)
	newRoot := initRuntimeProjectRepo(t)
	for _, root := range []string{oldRoot, newRoot} {
		if err := os.MkdirAll(filepath.Join(root, "agents", "coder"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "rules.md"), []byte("old project rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newRoot, "rules.md"), []byte("new project rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "agents", "coder", "persona.md"), []byte("old coder persona"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newRoot, "agents", "coder", "persona.md"), []byte("new coder persona"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug
	cfg.Agent.Active = "coder"
	cfg.Model.Port = modelPort
	cfg.Embedder.Port = freeTCPPort(t)

	rt := New(cfg, &runtimeConfigStore{cfg: &cfg, saved: true}, LogRings{})
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: oldRoot},
	}}
	rt.reqQueue = queue.New(1, nil)

	uiServer := ui.NewServer(0)
	rt.Start(context.Background(), uiServer, NewEventChannel(), nil)
	t.Cleanup(func() { stopRuntime(t, rt) })

	snap, release := rt.AcquireUISnapshot()
	defer release()
	if snap.ChatRunner == nil || snap.TaskRunner == nil {
		t.Fatal("snapshot missing chat or task runner")
	}

	// Reload to newRoot while holding the old snapshot.
	loaded := cfg
	loaded.Prompt.MemoryTokenBudget++
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: newRoot},
	}}
	store := &runtimeConfigStore{cfg: &loaded, saved: true}
	rt.cfgStore = store
	if !rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil).LiveApplied {
		t.Fatal("reload failed")
	}

	// Chat run through the captured runner. Draining the token stream to Done
	// guarantees the assembled request reached the model server.
	chatCtx, chatCancel := context.WithTimeout(context.Background(), 30*time.Second)
	chatID, tokens, err := snap.ChatRunner.Run(chatCtx, "coder", "", []ui.ChatMessage{{Role: "user", Content: "hello chat"}})
	if err != nil {
		chatCancel()
		t.Fatalf("captured chat run: %v", err)
	}
	for tok := range tokens {
		if tok.Err != nil {
			chatCancel()
			t.Fatalf("captured chat token error: %v", tok.Err)
		}
	}
	chatCancel()
	if chatID == "" {
		t.Fatal("captured chat run minted no session")
	}
	if _, err := snap.SessionStore.Save(context.Background(), chatID); err != nil {
		t.Fatalf("captured chat session save: %v", err)
	}

	// Task run through the captured runner.
	taskCtx, taskCancel := context.WithTimeout(context.Background(), 30*time.Second)
	taskID, events, err := snap.TaskRunner.RunTask(taskCtx, "coder", "", []ui.ChatMessage{{Role: "user", Content: "hello task"}})
	if err != nil {
		taskCancel()
		t.Fatalf("captured task run: %v", err)
	}
	for ev := range events {
		if ev.Type == agentloop.EvtError {
			taskCancel()
			t.Fatalf("captured task event error: %s", ev.Content)
		}
	}
	taskCancel()
	if taskID == "" {
		t.Fatal("captured task run minted no session")
	}
	if _, err := snap.SessionStore.Save(context.Background(), taskID); err != nil {
		t.Fatalf("captured task session save: %v", err)
	}

	// The captured runners must have assembled their prompts from the old
	// project's files — no request may contain the new project's rules.
	bodies := capture.snapshot()
	if len(bodies) == 0 {
		t.Fatal("model server received no completion requests")
	}
	sawOldRules := false
	for _, body := range bodies {
		if strings.Contains(body, "new project rules") {
			t.Fatalf("captured runner assembled with the new project's rules")
		}
		if strings.Contains(body, "old project rules") {
			sawOldRules = true
		}
	}
	if !sawOldRules {
		t.Fatal("captured runner never assembled with the old project's rules")
	}

	// Both sessions must be recorded exclusively in the original project.
	chatEp := filepath.Join("episodes", "coder", chatID+".md")
	chatJS := filepath.Join("episodes", "coder", chatID+".json")
	taskEp := filepath.Join("episodes", "coder", taskID+".md")
	taskJS := filepath.Join("episodes", "coder", taskID+".json")
	for _, rel := range []string{chatEp, chatJS, taskEp, taskJS} {
		if _, err := os.Stat(filepath.Join(oldRoot, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("captured session file missing from oldRoot %s: %v", rel, err)
		}
		if _, err := os.Stat(filepath.Join(newRoot, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("captured session file leaked into newRoot: %s", rel)
		}
	}
}

// TestSnapshot_ActiveAgentResolvedPerAcquisition verifies that the active
// agent is resolved per snapshot acquisition rather than frozen when the
// generation is built. /agents/active switches the active agent through the
// production registry without rebuilding the generation; a task posted with no
// explicit agent field must then run under the newly selected agent. Before
// the fix the task runner kept a build-time copy, so starting with no active
// agent left tasks failing with ErrTaskNoAgent even after one was selected.
func TestSnapshot_ActiveAgentResolvedPerAcquisition(t *testing.T) {
	modelPort, shutdownModel, capture := startCapturingModelServer(t, "test summary")
	// The model server must outlive the runtime's shutdown flush, or the
	// summarizer would fail to save the task's session during cleanup. Cleanups
	// run LIFO, so registering this first runs it after shutdown cleanup.
	t.Cleanup(shutdownModel)

	root := initRuntimeProjectRepo(t)
	if err := os.MkdirAll(filepath.Join(root, "agents", "coder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "coder", "persona.md"), []byte("coder persona"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug
	cfg.Agent.Active = "" // no active agent at startup
	cfg.Model.Port = modelPort
	cfg.Embedder.Port = freeTCPPort(t)

	rt := New(cfg, &runtimeConfigStore{cfg: &cfg, saved: true}, LogRings{})
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
	}}
	rt.reqQueue = queue.New(1, nil)

	port := freeTCPPort(t)
	uiServer := ui.NewServer(port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := uiServer.Start(ctx); err != nil {
		t.Fatalf("ui server start: %v", err)
	}
	rt.Start(context.Background(), uiServer, NewEventChannel(), nil)
	t.Cleanup(func() { stopRuntime(t, rt) })

	// Startup has no active agent.
	snap, release := rt.AcquireUISnapshot()
	if snap.ActiveAgent != "" {
		release()
		t.Fatalf("startup active agent = %q, want empty", snap.ActiveAgent)
	}
	// Switch the active agent through the production registry. This updates
	// the config store and rt.cfg without rebuilding the generation.
	if err := snap.AgentRegistry.SetActive("coder"); err != nil {
		release()
		t.Fatalf("SetActive: %v", err)
	}
	release()

	// A fresh acquisition reflects the switch with no rebuild.
	snap2, release2 := rt.AcquireUISnapshot()
	if snap2.ActiveAgent != "coder" {
		release2()
		t.Fatalf("active agent after switch = %q, want coder", snap2.ActiveAgent)
	}
	release2()

	// POST /task/send with no agent field: the handler must fall back to the
	// per-acquisition active agent and run the task under coder.
	resp, err := http.PostForm(fmt.Sprintf("http://127.0.0.1:%d/task/send", port), url.Values{"message": {"hello"}})
	if err != nil {
		t.Fatalf("task/send: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("task/send status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	select {
	case <-capture.first:
	case <-time.After(15 * time.Second):
		t.Fatal("task with no explicit agent never reached the model after an agent switch; the runner still used the stale empty active agent")
	}

	sawCoder := false
	for _, body := range capture.snapshot() {
		if strings.Contains(body, "coder persona") {
			sawCoder = true
		}
	}
	if !sawCoder {
		t.Fatal("task ran without the switched agent's persona; it did not fall back to the active agent")
	}
}

func TestShutdownWithInFlightLease(t *testing.T) {
	root := initRuntimeProjectRepo(t)
	if err := os.MkdirAll(filepath.Join(root, "agents", "coder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "coder", "persona.md"), []byte("coder persona"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "known.txt"), []byte("leased"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug
	cfg.Agent.Active = "coder"

	rt := New(cfg, nil, LogRings{})
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
	}}
	rt.reqQueue = queue.New(1, nil)
	rt.started = true

	uiServer := ui.NewServer(0)
	if !rt.startMemoryAndAPI(context.Background(), uiServer, nil, &rt.cfg) {
		t.Fatal("initial start failed")
	}

	asm, _, _, release := rt.AcquireRequestGeneration()

	// Shutdown while the lease is held.
	stopRuntime(t, rt)

	// The lease protects the captured generation. Assemble must
	// still work — the generation's readers are pinned.
	_, err := asm.Assemble(context.Background(), "coder", []inference.Message{{Role: "user", Content: "read known"}})
	if err != nil {
		release()
		t.Fatalf("assemble after Shutdown with held lease: %v", err)
	}

	// The root must still be pinned on Windows.
	if err := os.RemoveAll(root); err == nil {
		if runtime.GOOS == "windows" {
			release()
			t.Fatal("root was removable despite held lease after Shutdown")
		}
	}

	// Release drops the last lease. The old readers close.
	release()

	// Second Shutdown is a no-op.
	stopRuntime(t, rt)
}
