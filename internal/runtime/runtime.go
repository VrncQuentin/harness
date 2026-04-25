// Package runtime owns the mutable service graph behind the harness.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/api"
	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/db"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/logbuf"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/metrics"
	"github.com/vrnc/harness/internal/proc"
	"github.com/vrnc/harness/internal/prompt"
	"github.com/vrnc/harness/internal/queue"
	"github.com/vrnc/harness/internal/ui"
	"github.com/vrnc/harness/pkg/httpclient"
)

// ErrConfigStoreUnavailable is surfaced when the harness DB could not be
// opened, so the user sees one consistent message in the status page and the
// config editor.
var ErrConfigStoreUnavailable = errors.New("config store unavailable (harness.db could not be opened)")

// EventBufferSize absorbs process-manager startup bursts without forcing
// managers to block while the UI event forwarder catches up.
const EventBufferSize = 64

// Rings groups the in-memory log buffers shared by services and the UI.
type Rings struct {
	Log   *logbuf.Ring
	Llama *logbuf.Ring
	Embed *logbuf.Ring
}

// Runtime holds mutable service references that the retry/save callback
// reconfigures in place. A mutex guards all fields because callbacks run on
// HTTP goroutines while event forwarding and metrics read managers and queue.
type Runtime struct {
	mu        sync.Mutex
	cfg       config.Config
	cfgStore  config.Store
	logRing   *logbuf.Ring
	llamaRing *logbuf.Ring
	embedRing *logbuf.Ring
	llamaMgr  *proc.Manager
	embedMgr  *proc.Manager
	reqQueue  *queue.Queue
	started   bool

	memReader *memory.DirReader
	agentReg  *agent.DiskRegistry
	assembler *prompt.DiskAssembler
	hotReload *prompt.HotReload
	apiServer *api.Server
}

// New returns a runtime seeded with the loaded config and shared log rings.
func New(cfg config.Config, cfgStore config.Store, rings Rings) *Runtime {
	return &Runtime{
		cfg:       cfg,
		cfgStore:  cfgStore,
		logRing:   rings.Log,
		llamaRing: rings.Llama,
		embedRing: rings.Embed,
	}
}

// NewEventChannel returns the process event channel shared by all managers.
func NewEventChannel() chan proc.Event {
	return make(chan proc.Event, EventBufferSize)
}

// OpenDB opens harness.db (running migrations + seed) and returns the handle
// plus the typed sub-stores. Any failure is surfaced to the UI as a startup
// error; the returned handle and stores may be nil, which callers must handle.
func OpenDB(uiServer *ui.Server, path string) (*db.DB, config.Store, metrics.Store) {
	d, err := db.Open(path)
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("harness.db: %w", err))
		return nil, nil, nil
	}
	uiServer.SetConfigStore(d.Config())
	return d, d.Config(), d.Metrics()
}

// ValidatePaths checks that the binaries and model files referenced by cfg
// exist on disk and surfaces any missing ones as startup errors.
func ValidatePaths(uiServer *ui.Server, cfg *config.Config) {
	checks := []struct {
		label, path string
	}{
		{"model file", cfg.Model.ModelPath},
		{"llama-server binary", cfg.Model.Binary},
		{"embedder binary", cfg.Embedder.Binary},
		{"embedder model file", cfg.Embedder.ModelPath},
	}
	for _, c := range checks {
		if c.path == "" {
			continue
		}
		if _, err := os.Stat(c.path); errors.Is(err, fs.ErrNotExist) {
			uiServer.AddStartupError(fmt.Errorf("%s not found: %s", c.label, c.path))
		}
	}
}

// Start brings llama-server, embedder, queue, memory, prompt, and API services
// up under the current config.
func (rt *Runtime) Start(
	ctx context.Context,
	uiServer *ui.Server,
	events chan proc.Event,
	metricsStore metrics.Store,
) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.startServices(ctx, uiServer, events, metricsStore)
	rt.startMemoryAndAPI(ctx, uiServer)
}

// ApplyConfig reloads config from the store, validates it, and either starts
// services for the first time or reconfigures the live ones to match. Tier-3
// changes (UI port, queue) are returned as RestartNeeded so the UI can flag
// them - no live apply path exists for those yet.
func (rt *Runtime) ApplyConfig(
	ctx context.Context,
	uiServer *ui.Server,
	events chan proc.Event,
	metricsStore metrics.Store,
) ui.ApplyResult {
	uiServer.ClearStartupErrors()
	if rt.cfgStore == nil {
		uiServer.AddStartupError(ErrConfigStoreUnavailable)
		return ui.ApplyResult{}
	}
	loaded, wasSaved, lerr := rt.cfgStore.Load()
	if lerr != nil {
		uiServer.AddStartupError(fmt.Errorf("config load: %w", lerr))
		return ui.ApplyResult{}
	}
	uiServer.SetFirstRun(!wasSaved)
	if !wasSaved {
		return ui.ApplyResult{}
	}
	if verr := config.Validate(loaded); verr != nil {
		uiServer.AddStartupError(verr)
		return ui.ApplyResult{}
	}
	ValidatePaths(uiServer, loaded)

	rt.mu.Lock()
	defer rt.mu.Unlock()

	old := rt.cfg
	rt.cfg = *loaded

	var result ui.ApplyResult

	if !rt.started {
		slog.Info("starting services", "model_port", loaded.Model.Port, "embed_port", loaded.Embedder.Port)
		rt.startServices(ctx, uiServer, events, metricsStore)
		rt.startMemoryAndAPI(ctx, uiServer)
		result.LiveApplied = true
	} else {
		if old.Model != loaded.Model {
			slog.Info("reconfiguring llama-server", "old_port", old.Model.Port, "new_port", loaded.Model.Port)
			rt.llamaMgr.Reconfigure(func() (string, []string) {
				return proc.LlamaArgs(
					loaded.Model.Binary,
					loaded.Model.ModelPath,
					loaded.Model.CtxSize,
					loaded.Model.GPULayers,
					loaded.Model.NParallel,
					loaded.Model.Port,
					loaded.Model.Verbose,
				)
			}, fmt.Sprintf("http://127.0.0.1:%d/health", loaded.Model.Port))
			result.LiveApplied = true
		}
		if old.Embedder != loaded.Embedder {
			slog.Info("reconfiguring embedder", "old_port", old.Embedder.Port, "new_port", loaded.Embedder.Port)
			rt.embedMgr.Reconfigure(func() (string, []string) {
				return proc.EmbedderArgs(
					loaded.Embedder.Binary,
					loaded.Embedder.ModelPath,
					loaded.Embedder.Port,
					loaded.Embedder.Verbose,
				)
			}, fmt.Sprintf("http://127.0.0.1:%d/health", loaded.Embedder.Port))
			result.LiveApplied = true
		}
		if old.Model.Port != loaded.Model.Port && rt.reqQueue != nil {
			rt.reqQueue.SetClient(inference.NewClient(
				fmt.Sprintf("http://127.0.0.1:%d", loaded.Model.Port),
				httpclient.NewStreaming(),
			))
		}

		if old.Memory != loaded.Memory ||
			old.Prompt != loaded.Prompt ||
			old.API != loaded.API ||
			old.Agent.Active != loaded.Agent.Active {
			slog.Info("rebuilding memory and api services")
			rt.stopMemoryAndAPI(uiServer)
			rt.startMemoryAndAPI(ctx, uiServer)
			result.LiveApplied = true
		}
	}

	if old.UI.Port != loaded.UI.Port {
		result.RestartNeeded = append(result.RestartNeeded, "UI port")
	}
	if old.Queue.MaxDepth != loaded.Queue.MaxDepth {
		result.RestartNeeded = append(result.RestartNeeded, "queue max depth")
	}
	if old.Queue.WALPath != loaded.Queue.WALPath {
		result.RestartNeeded = append(result.RestartNeeded, "queue WAL path")
	}
	if old.Log.RingMaxEntries != loaded.Log.RingMaxEntries && rt.logRing != nil {
		rt.logRing.Resize(loaded.Log.RingMaxEntries)
		result.LiveApplied = true
	}
	if old.Log.ProcMaxLines != loaded.Log.ProcMaxLines {
		if rt.llamaRing != nil {
			rt.llamaRing.Resize(loaded.Log.ProcMaxLines)
		}
		if rt.embedRing != nil {
			rt.embedRing.Resize(loaded.Log.ProcMaxLines)
		}
		result.LiveApplied = true
	}

	return result
}

// Managers returns the process managers currently owned by the runtime.
func (rt *Runtime) Managers() (*proc.Manager, *proc.Manager) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.llamaMgr, rt.embedMgr
}

// RestartLlama restarts the current llama-server manager when one exists.
func (rt *Runtime) RestartLlama() {
	if m, _ := rt.Managers(); m != nil {
		m.Restart()
	}
}

// RestartEmbedder restarts the current embedder manager when one exists.
func (rt *Runtime) RestartEmbedder() {
	if _, m := rt.Managers(); m != nil {
		m.Restart()
	}
}

// Stop tears down runtime-owned services that need explicit shutdown.
func (rt *Runtime) Stop() {
	rt.mu.Lock()
	q := rt.reqQueue
	apiSrv := rt.apiServer
	hr := rt.hotReload
	rt.mu.Unlock()

	if apiSrv != nil {
		apiSrv.Stop()
	}
	if q != nil {
		q.Stop()
	}
	if hr != nil {
		if err := hr.Close(); err != nil {
			slog.Warn("prompt hot-reload close", "err", err)
		}
	}
}

// startServices brings llama-server, embedder, queue, and metrics up under the
// current rt.cfg. Caller must hold rt.mu.
func (rt *Runtime) startServices(
	ctx context.Context,
	uiServer *ui.Server,
	events chan proc.Event,
	metricsStore metrics.Store,
) {
	cfg := &rt.cfg

	rt.llamaMgr = proc.NewManager(proc.ManagerConfig{
		Name: "llama-server",
		BuildArgs: func() (string, []string) {
			return proc.LlamaArgs(
				cfg.Model.Binary,
				cfg.Model.ModelPath,
				cfg.Model.CtxSize,
				cfg.Model.GPULayers,
				cfg.Model.NParallel,
				cfg.Model.Port,
				cfg.Model.Verbose,
			)
		},
		HealthURL:   fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Model.Port),
		Events:      events,
		CheckPeriod: 5 * time.Second,
		HTTPClient:  httpclient.New(),
		Output:      rt.llamaRing,
	})
	go rt.llamaMgr.Run(ctx)

	rt.embedMgr = proc.NewManager(proc.ManagerConfig{
		Name: "embedder",
		BuildArgs: func() (string, []string) {
			return proc.EmbedderArgs(
				cfg.Embedder.Binary,
				cfg.Embedder.ModelPath,
				cfg.Embedder.Port,
				cfg.Embedder.Verbose,
			)
		},
		HealthURL:   fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Embedder.Port),
		Events:      events,
		CheckPeriod: 5 * time.Second,
		HTTPClient:  httpclient.New(),
		Output:      rt.embedRing,
	})
	go rt.embedMgr.Run(ctx)

	inferClient := inference.NewClient(
		fmt.Sprintf("http://127.0.0.1:%d", cfg.Model.Port),
		httpclient.NewStreaming(),
	)
	rt.reqQueue = queue.New(cfg.Queue.MaxDepth, cfg.Queue.WALPath, inferClient)
	if err := rt.reqQueue.Start(ctx); err != nil {
		uiServer.AddStartupError(fmt.Errorf("queue WAL error: %w", err))
	}

	if metricsStore != nil {
		go recordMetrics(ctx, metricsStore, rt.llamaMgr, rt.embedMgr, rt.reqQueue)
	}

	rt.started = true
}

// startMemoryAndAPI brings up the memory reader, agent registry, prompt
// assembler, hot-reload watcher, and API server. Caller must hold rt.mu.
func (rt *Runtime) startMemoryAndAPI(ctx context.Context, uiServer *ui.Server) {
	uiServer.SetMemoryRepoPath(rt.cfg.Memory.RepoPath)
	if err := memory.ValidateRepo(rt.cfg.Memory.RepoPath); err != nil {
		uiServer.SetAgentRegistry(nil)
		uiServer.SetMemoryStore(nil)
		uiServer.AddStartupError(fmt.Errorf("memory repo: %w", err))
		if rt.cfg.API.Enabled {
			uiServer.AddStartupError(errors.New("api server disabled: memory repo is not valid"))
		}
		return
	}

	rt.memReader = memory.NewDirReader(rt.cfg.Memory.RepoPath)
	rt.agentReg = agent.NewDiskRegistry(rt.memReader, rt.getActive, rt.setActive)
	rt.assembler = prompt.NewDiskAssembler(rt.memReader, rt.agentReg, rt.cfg.Prompt)
	uiServer.SetMemoryStore(rt.memReader)

	hr, err := prompt.NewHotReload(rt.cfg.Memory.RepoPath, rt.cfg.Agent.Active, slog.Default())
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("prompt hot-reload: %w", err))
	} else {
		rt.hotReload = hr
	}

	uiServer.SetAgentRegistry(&uiAgentRegistryAdapter{reg: rt.agentReg, mem: rt.memReader})

	if rt.cfg.API.Enabled && rt.reqQueue != nil {
		srv := api.NewServer(rt.cfg.API.Port, &apiAssemblerAdapter{a: rt.assembler, rt: rt}, rt.reqQueue)
		if err := srv.Start(ctx); err != nil {
			uiServer.AddStartupError(fmt.Errorf("api server: %w", err))
		} else {
			rt.apiServer = srv
			slog.Info("api server listening", "port", rt.cfg.API.Port)
		}
	}
}

// stopMemoryAndAPI tears down the M2 services. Caller must hold rt.mu.
func (rt *Runtime) stopMemoryAndAPI(uiServer *ui.Server) {
	if rt.apiServer != nil {
		rt.apiServer.Stop()
		rt.apiServer = nil
	}
	if rt.hotReload != nil {
		if err := rt.hotReload.Close(); err != nil {
			slog.Warn("prompt hot-reload close", "err", err)
		}
		rt.hotReload = nil
	}
	rt.memReader = nil
	rt.agentReg = nil
	rt.assembler = nil
	uiServer.SetAgentRegistry(nil)
	uiServer.SetMemoryStore(nil)
}

func (rt *Runtime) getActive() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.cfg.Agent.Active
}

func (rt *Runtime) setActive(name string) error {
	rt.mu.Lock()
	store := rt.cfgStore
	hr := rt.hotReload
	rt.mu.Unlock()

	if store == nil {
		return ErrConfigStoreUnavailable
	}
	loaded, _, err := store.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	loaded.Agent.Active = name
	if err := store.Save(loaded); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	rt.mu.Lock()
	rt.cfg.Agent.Active = name
	rt.mu.Unlock()

	if hr != nil {
		hr.SetActiveAgent(name)
	}
	return nil
}

type uiAgentRegistryAdapter struct {
	reg agent.Registry
	mem memory.Reader
}

func (ad *uiAgentRegistryAdapter) List() ([]ui.AgentInfo, error) {
	agents, err := ad.reg.List()
	if err != nil {
		return nil, err
	}
	out := make([]ui.AgentInfo, 0, len(agents))
	for _, a := range agents {
		info := ui.AgentInfo{
			Name:        a.Name,
			PersonaPath: a.PersonaPath,
			RulesPath:   a.RulesPath,
			NotesPath:   a.NotesPath,
		}
		if persona, err := readOptional(ad.mem, a.PersonaPath); err == nil {
			info.Persona = persona
		}
		if rules, err := readOptional(ad.mem, a.RulesPath); err == nil {
			info.Rules = rules
		}
		if notes, err := readOptional(ad.mem, a.NotesPath); err == nil {
			info.Notes = notes
		}
		out = append(out, info)
	}
	return out, nil
}

func (ad *uiAgentRegistryAdapter) Get(name string) (ui.AgentInfo, error) {
	a, err := ad.reg.Get(name)
	if err != nil {
		return ui.AgentInfo{}, err
	}
	info := ui.AgentInfo{
		Name:        a.Name,
		PersonaPath: a.PersonaPath,
		RulesPath:   a.RulesPath,
		NotesPath:   a.NotesPath,
	}
	persona, err := readOptional(ad.mem, a.PersonaPath)
	if err != nil {
		return info, err
	}
	info.Persona = persona
	rules, err := readOptional(ad.mem, a.RulesPath)
	if err != nil {
		return info, err
	}
	info.Rules = rules
	notes, err := readOptional(ad.mem, a.NotesPath)
	if err != nil {
		return info, err
	}
	info.Notes = notes
	return info, nil
}

func readOptional(mem memory.Reader, relPath string) (string, error) {
	b, err := mem.Read(relPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}

func (ad *uiAgentRegistryAdapter) Active() string {
	return ad.reg.Active()
}

func (ad *uiAgentRegistryAdapter) SetActive(name string) error {
	return ad.reg.SetActive(name)
}

func (ad *uiAgentRegistryAdapter) Create(name string) error {
	_, err := ad.reg.Create(name)
	return err
}

func (ad *uiAgentRegistryAdapter) WritePersona(name string, body []byte) error {
	return ad.reg.WritePersona(name, body)
}

func (ad *uiAgentRegistryAdapter) WriteRules(name string, body []byte) error {
	return ad.reg.WriteRules(name, body)
}

func (ad *uiAgentRegistryAdapter) WriteNotes(name string, body []byte) error {
	return ad.reg.WriteNotes(name, body)
}

func (ad *uiAgentRegistryAdapter) Delete(name string) error {
	return ad.reg.Delete(name)
}

var errNoActiveAgent = errors.New("api: no agent specified and no active agent configured (set one in /agents)")

type apiAssemblerAdapter struct {
	a  *prompt.DiskAssembler
	rt *Runtime
}

func (ad *apiAssemblerAdapter) Assemble(ctx context.Context, agentName string, conversation []inference.Message) ([]inference.Message, error) {
	if agentName == "" {
		agentName = ad.rt.getActive()
	}
	if agentName == "" {
		return nil, errNoActiveAgent
	}
	msgs, _, err := ad.a.Assemble(ctx, agentName, conversation)
	return msgs, err
}
