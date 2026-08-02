package runtime

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VrncQuentin/harness/internal/api"
	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/proc"
	"github.com/VrncQuentin/harness/internal/project"
	"github.com/VrncQuentin/harness/internal/queue"
	"github.com/VrncQuentin/harness/internal/ui"
)

// appliedRuntimeForTest builds a runtime with a live generation over a seeded
// project repo and the given config, records its applied state, installs llama
// and embedder managers configured for cfg.Model, and returns the runtime plus
// the project store. The store initially holds cfg.
func appliedRuntimeForTest(t *testing.T, cfg *config.Config, projects *runtimeProjectStoreStub) (*Runtime, *runtimeProjectStoreStub) {
	t.Helper()
	root := initRuntimeProjectRepo(t)
	if projects == nil {
		projects = &runtimeProjectStoreStub{projects: map[string]project.Project{
			project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
		}}
	}
	store := &runtimeConfigStore{cfg: cfg, saved: true}
	rt := New(*cfg, store, LogRings{})
	rt.projectStore = projects
	rt.reqQueue = queue.New(4, nil)
	uiServer := ui.NewServer(0)
	if !rt.startMemoryAndAPI(context.Background(), uiServer, nil, cfg) {
		t.Fatal("initial memory services failed")
	}
	rt.started = true
	// The fake managers are configured from the recorded running model, not
	// the global cfg.Model: when the store supplies a project override the
	// helper must pretend the process already runs that override, or tests
	// that assert a reconfiguration would be proving a transition that never
	// happened.
	runningModel := rt.applied.runningModel
	rt.llamaMgr = proc.NewManager(proc.ManagerConfig{
		Name:      "llama-server",
		BuildArgs: func() (string, []string) { return llamaArgsForModel(runningModel) },
		HealthURL: llamaHealthURL(runningModel),
	})
	rt.embedMgr = proc.NewManager(proc.ManagerConfig{
		Name:      "embedder",
		BuildArgs: func() (string, []string) { return embedderArgsForConfig(rt.applied.runningEmbedder) },
		HealthURL: embedderHealthURL(rt.applied.runningEmbedder),
	})
	t.Cleanup(func() { rt.Stop() })
	return rt, projects
}

// TestAppliedState_OldModelNotReconstructedFromStore verifies that the old
// model an apply compares against comes from the recorded applied state, not
// reconstructed from the mutable project store. The runtime applies model A,
// the store then starts preferring model B, and a config apply must detect the
// change (A != B) and retarget llama — proving the comparison did not re-derive
// the old model from the store (which would have read B and suppressed the
// reconfigure).
func TestAppliedState_OldModelNotReconstructedFromStore(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, projects := appliedRuntimeForTest(t, &cfg, nil)
	if rt.applied == nil || rt.applied.runningModel.ModelPath != cfg.Model.ModelPath {
		t.Fatalf("initial applied running model = %+v, want model A", rt.applied)
	}

	binaryB := "project-llama-b"
	modelB := "project-b.gguf"
	projects.projects[project.GlobalSlug] = project.Project{
		Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: projects.projects[project.GlobalSlug].MemoryRepoPath,
		ModelBinary: &binaryB, ModelPath: &modelB,
	}

	loaded := cfg
	loaded.Prompt.MemoryTokenBudget++
	rt.cfgStore = &runtimeConfigStore{cfg: &loaded, saved: true}

	uiServer := ui.NewServer(0)
	if !rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil).LiveApplied {
		t.Fatal("override change should apply live")
	}

	if rt.applied == nil || rt.applied.runningModel.ModelPath != modelB {
		t.Fatalf("applied running model = %+v, want model B; the old model was reconstructed from the store instead of the recorded applied state", rt.applied)
	}
	bin, args, _ := rt.llamaMgr.Args()
	if bin != binaryB {
		t.Fatalf("llama binary = %q, want %q", bin, binaryB)
	}
	if !strings.Contains(strings.Join(args, " "), modelB) {
		t.Fatalf("llama args %v do not reference model B", args)
	}
}

// TestAppliedState_SerializedApplyConfig verifies that two concurrent applies
// serialize end-to-end: the second apply cannot enter validation or
// preparation until the first has fully committed. The enter/leave seams record
// the lock-held regions; an apply that interleaved would emit enter before the
// first leave.
func TestAppliedState_SerializedApplyConfig(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, _ := appliedRuntimeForTest(t, &cfg, nil)
	var seqMu sync.Mutex
	var seq []string
	var once sync.Once
	entered := make(chan struct{})
	allow := make(chan struct{})
	rt.enterApply = func() {
		seqMu.Lock()
		seq = append(seq, "enter")
		seqMu.Unlock()
	}
	rt.leaveApply = func() {
		seqMu.Lock()
		seq = append(seq, "leave")
		seqMu.Unlock()
	}
	rt.afterPrepare = func() {
		once.Do(func() {
			close(entered)
			<-allow
		})
	}

	loaded := cfg
	loaded.Prompt.MemoryTokenBudget++
	rt.cfgStore = &runtimeConfigStore{cfg: &loaded, saved: true}
	uiServer := ui.NewServer(0)

	firstDone := make(chan bool, 1)
	go func() {
		res := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil)
		firstDone <- res.LiveApplied
	}()

	// Apply #1 has prepared and holds applyMu; apply #2 must be blocked before
	// its enterApply.
	<-entered
	secondDone := make(chan bool, 1)
	go func() {
		res := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil)
		secondDone <- res.LiveApplied
	}()

	close(allow)
	if !<-firstDone {
		t.Fatal("first apply failed")
	}
	<-secondDone

	seqMu.Lock()
	defer seqMu.Unlock()
	want := []string{"enter", "leave", "enter", "leave"}
	if !reflect.DeepEqual(seq, want) {
		t.Fatalf("apply sequence = %v, want %v (applies must not interleave)", seq, want)
	}
}

// TestAppliedState_PrepareQuiesceCommitRetire verifies that the apply
// transaction phases are real: the candidate is prepared locally and stays
// unpublished until commit, then the new generation and applied state are
// installed together. During preparation the old generation keeps serving.
func TestAppliedState_PrepareQuiesceCommitRetire(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, _ := appliedRuntimeForTest(t, &cfg, nil)
	oldGen := rt.gen
	if oldGen == nil {
		t.Fatal("initial generation missing")
	}
	oldApplied := rt.applied
	if oldApplied == nil {
		t.Fatal("initial applied state missing")
	}

	prepared := make(chan struct{})
	resume := make(chan struct{})
	rt.afterPrepare = func() {
		close(prepared)
		<-resume
	}

	loaded := cfg
	loaded.Prompt.MemoryTokenBudget++
	rt.cfgStore = &runtimeConfigStore{cfg: &loaded, saved: true}
	uiServer := ui.NewServer(0)

	done := make(chan ui.ApplyResult, 1)
	go func() {
		done <- rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil)
	}()

	// The candidate is prepared but NOT published until commit: the old
	// generation and applied state are still installed and serving.
	<-prepared
	rt.mu.Lock()
	published := rt.gen
	appliedNow := rt.applied
	rt.mu.Unlock()
	if published != oldGen {
		t.Fatal("candidate was published before commit")
	}
	if appliedNow != oldApplied {
		t.Fatal("applied state was replaced before commit")
	}
	snap, release := rt.AcquireUISnapshot()
	if snap.SessionStore == nil || snap.MemoryRepoPath == "" {
		release()
		t.Fatal("snapshot during preparation did not serve the old generation")
	}
	release()

	close(resume)
	res := <-done
	if !res.LiveApplied {
		t.Fatal("apply did not report live apply")
	}

	rt.mu.Lock()
	committed := rt.gen
	committedApplied := rt.applied
	rt.mu.Unlock()
	if committed == oldGen {
		t.Fatal("commit did not publish the new generation")
	}
	if committedApplied == nil || committedApplied == oldApplied ||
		committedApplied.cfg.Prompt.MemoryTokenBudget != loaded.Prompt.MemoryTokenBudget {
		t.Fatalf("committed applied state wrong: %+v", committedApplied)
	}
}

// TestAppliedState_FailedCandidateDiscarded verifies that a failed candidate is
// discarded as one object: the installed generation and recorded applied state
// stay usable and untouched, and a later valid apply still succeeds.
func TestAppliedState_FailedCandidateDiscarded(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, _ := appliedRuntimeForTest(t, &cfg, nil)
	oldGen := rt.gen
	oldApplied := rt.applied
	oldSession := rt.SessionManager()
	oldMem := rt.activeMem
	if oldGen == nil || oldApplied == nil || oldSession == nil {
		t.Fatal("initial generation/applied/session missing")
	}

	// Corrupt the episode index manifest so the next candidate fails to build
	// after its readers are open.
	repoPath := projectsPathForApplied(t, rt)
	manifestDir := filepath.Join(repoPath, "index", "_episodes")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.json"), []byte("{not json}"), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded := cfg
	loaded.Prompt.MemoryTokenBudget++
	rt.cfgStore = &runtimeConfigStore{cfg: &loaded, saved: true}

	uiServer := ui.NewServer(0)
	if result := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil); result.LiveApplied {
		t.Fatal("apply with corrupt candidate must not report live apply")
	}
	if rt.gen != oldGen {
		t.Fatal("installed generation was replaced by a failed candidate")
	}
	if rt.applied != oldApplied {
		t.Fatal("recorded applied state was replaced by a failed candidate")
	}
	if rt.SessionManager() != oldSession {
		t.Fatal("session manager was replaced by a failed candidate")
	}
	if rt.activeMem != oldMem {
		t.Fatal("active memory reader was replaced by a failed candidate")
	}
	// The installed generation remains usable.
	if _, err := rt.activeMem.Read("rules.md"); err != nil {
		t.Fatalf("installed generation reader failed after discarded candidate: %v", err)
	}
	snap, release := rt.AcquireUISnapshot()
	if snap.SessionStore == nil {
		release()
		t.Fatal("snapshot not served after discarded candidate")
	}
	release()

	// A subsequent valid apply still works — the discarded candidate left no
	// partial state behind.
	if err := os.Remove(filepath.Join(manifestDir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if !rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil).LiveApplied {
		t.Fatal("apply after discarded candidate failed")
	}
	if rt.applied == oldApplied {
		t.Fatal("recovery apply did not install the new applied state")
	}
}

// projectsPathForApplied returns the active project's memory repo path from the
// runtime's project store.
func projectsPathForApplied(t *testing.T, rt *Runtime) string {
	t.Helper()
	if rt.projectStore == nil {
		t.Fatal("project store missing")
	}
	p, err := rt.projectStore.Get(rt.applied.activeSlug)
	if err != nil {
		t.Fatalf("project store Get: %v", err)
	}
	return p.MemoryRepoPath
}

// TestAppliedState_RollbackUsesRecordedState verifies that no process change is
// issued before the candidate is known good, and that the recorded applied
// state is what any rollback would use. A project switch to a project whose
// memory repo is invalid must leave llama-server configured exactly as the
// recorded applied state describes — not re-pointed at the uncommitted
// preferred model.
func TestAppliedState_RollbackUsesRecordedState(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, projects := appliedRuntimeForTest(t, &cfg, nil)
	modelA := cfg.Model
	if rt.applied == nil || rt.applied.runningModel != modelA {
		t.Fatalf("initial applied running model = %+v, want model A", rt.applied)
	}

	// The destination project prefers model B but its memory repo is not a
	// valid project repo, so candidate construction must fail.
	binaryB := "project-llama-b"
	modelB := "project-b.gguf"
	projects.projects["demo"] = project.Project{
		Slug: "demo", DisplayName: "Demo", MemoryRepoPath: t.TempDir(),
		ModelBinary: &binaryB, ModelPath: &modelB,
	}

	loaded := cfg
	loaded.Project.ActiveProjectSlug = "demo"
	rt.cfgStore = &runtimeConfigStore{cfg: &loaded, saved: true}

	result := rt.ApplyConfig(context.Background(), ui.NewServer(0), NewEventChannel(), nil)
	if result.LiveApplied {
		t.Fatal("failed apply must not report live apply")
	}

	bin, args, _ := rt.llamaMgr.Args()
	if strings.Contains(strings.Join(args, " "), modelB) || bin == binaryB {
		t.Fatalf("llama was reconfigured to the uncommitted model before the candidate was known good: bin=%q args=%v", bin, args)
	}
	if rt.applied == nil || rt.applied.runningModel != modelA {
		t.Fatalf("recorded applied running model = %+v, want model A (rollback must use the recorded state)", rt.applied)
	}
}

// TestAppliedState_LiveAppliedReflectsFinalState verifies that
// ui.ApplyResult.LiveApplied describes the final live state: a model-only
// reconfigure reports true because the live process was retargeted, while a
// failed apply reports false and leaves the live process and recorded state
// untouched.
func TestAppliedState_LiveAppliedReflectsFinalState(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, projects := appliedRuntimeForTest(t, &cfg, nil)
	// A model-only change (no config field, no rebuild) must report live
	// apply because the live process is reconfigured to the new effective
	// model from the store override.
	binaryB := "project-llama-b"
	modelB := "project-b.gguf"
	projects.projects[project.GlobalSlug] = project.Project{
		Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: projects.projects[project.GlobalSlug].MemoryRepoPath,
		ModelBinary: &binaryB, ModelPath: &modelB,
	}
	uiServer := ui.NewServer(0)
	if result := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil); !result.LiveApplied {
		t.Fatal("model-only reconfigure did not report live apply")
	}
	bin, args, _ := rt.llamaMgr.Args()
	if !strings.Contains(strings.Join(args, " "), modelB) {
		t.Fatalf("llama not reconfigured to model B despite LiveApplied: %v", args)
	}
	_ = bin

	// A failed apply must report no live apply and leave the live process and
	// recorded state untouched: LiveApplied=false must describe a live state
	// that really did not change.
	loaded := cfg
	loaded.Project.ActiveProjectSlug = "missing"
	rt.cfgStore = &runtimeConfigStore{cfg: &loaded, saved: true}
	if result := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil); result.LiveApplied {
		t.Fatal("failed apply reported live apply")
	}
	if rt.applied == nil || rt.applied.runningModel.ModelPath != modelB {
		t.Fatalf("failed apply disturbed the recorded state: %+v", rt.applied)
	}
	_, args, _ = rt.llamaMgr.Args()
	if !strings.Contains(strings.Join(args, " "), modelB) {
		t.Fatalf("failed apply changed the live process: %v (LiveApplied=false must describe an unchanged live state)", args)
	}

	// A metrics-only change is live: the retention callback reads rt.cfg
	// dynamically, so committing a new retention changes live behavior even
	// though no service is rebuilt.
	metricsOnly := cfg
	metricsOnly.Metrics.RetentionDays = cfg.Metrics.RetentionDays + 1
	rt.cfgStore = &runtimeConfigStore{cfg: &metricsOnly, saved: true}
	if result := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil); !result.LiveApplied {
		t.Fatal("metrics-only change should report a live apply")
	}
	if rt.applied == nil || rt.applied.cfg.Metrics.RetentionDays != metricsOnly.Metrics.RetentionDays {
		t.Fatalf("metrics-only change not recorded: %+v", rt.applied)
	}
}

// TestAppliedState_ActiveAgentWriteSerializedWithApply verifies that the
// /agents/active out-of-band mutation participates in the apply transaction:
// an apply that has loaded one agent and is preparing cannot be overwritten by
// a concurrent active-agent save, and vice versa. After both operations the
// live config, the recorded applied config, and the store must agree.
//
// The test is barrier-driven and deterministic: a before-lock hook proves the
// writer reached the apply transaction lock while the apply is paused holding
// it, and an instrumented-store seam proves the writer's Save cannot land
// while the apply is paused. Without the production lock the writer's Save
// fires during the pause and the test fails; with it, the writer blocks at the
// lock until the apply commits.
func TestAppliedState_ActiveAgentWriteSerializedWithApply(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug
	cfg.Agent.Active = "coder"

	rt, _ := appliedRuntimeForTest(t, &cfg, nil)

	prepared := make(chan struct{})
	resume := make(chan struct{})
	rt.afterPrepare = func() {
		close(prepared)
		<-resume
	}

	// The apply loads agent "coder" and pauses in preparation while holding
	// the apply transaction lock.
	loaded := cfg
	loaded.Prompt.MemoryTokenBudget++
	store := &runtimeConfigStore{cfg: &loaded, saved: true}
	rt.cfgStore = store

	// Barrier A: the writer reached the transaction lock boundary.
	writerAtLock := make(chan struct{})
	rt.beforeApplyMu = func() { close(writerAtLock) }
	// Barrier B: the writer's store Save landed. If it lands while the apply
	// is paused, the writer bypassed the lock (regression).
	writerSaved := make(chan struct{}, 1)
	store.onSave = func() { close(writerSaved) }

	applyDone := make(chan bool, 1)
	go func() {
		res := rt.ApplyConfig(context.Background(), ui.NewServer(0), NewEventChannel(), nil)
		applyDone <- res.LiveApplied
	}()

	<-prepared
	agentDone := make(chan error, 1)
	go func() {
		agentDone <- rt.setActiveAgent("coderB")
	}()
	<-writerAtLock

	// With the lock present the writer is blocked at applyMu (the apply holds
	// it and is paused), so its Save cannot fire here. If the lock were
	// removed, the writer's Save would land during the pause and the apply's
	// later commit would overwrite the live state with the stale loaded agent.
	select {
	case <-writerSaved:
		t.Fatal("active-agent write landed while the apply was paused; the write must serialize with the apply")
	case <-time.After(50 * time.Millisecond):
	}

	close(resume)
	if !<-applyDone {
		t.Fatal("apply failed")
	}
	if err := <-agentDone; err != nil {
		t.Fatalf("setActiveAgent: %v", err)
	}

	storedCfg, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	rt.mu.Lock()
	liveAgent := rt.cfg.Agent.Active
	appliedAgent := rt.applied.cfg.Agent.Active
	rt.mu.Unlock()
	if liveAgent != "coderB" || appliedAgent != "coderB" || storedCfg.Agent.Active != "coderB" {
		t.Fatalf("active agent diverged: live=%q applied=%q store=%q (the write must serialize with the apply)", liveAgent, appliedAgent, storedCfg.Agent.Active)
	}
}

// TestAppliedState_MissingAPIServerRebuilt verifies that an apply rebuilds a
// missing API listener even when the recorded applied config is unchanged:
// rebuild can be forced by memoryAPIUnavailable (rt.apiServer == nil while the
// applied config wants the API running), so the build decision must consider
// actual listener ownership, not config diffs alone.
func TestAppliedState_MissingAPIServerRebuilt(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug
	cfg.API.Enabled = true
	cfg.API.Port = freeTCPPort(t)

	root := initRuntimeProjectRepo(t)
	store := &runtimeConfigStore{cfg: &cfg, saved: true}
	projects := &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
	}}
	rt := New(cfg, store, LogRings{})
	rt.projectStore = projects
	rt.reqQueue = queue.New(4, nil)
	uiServer := ui.NewServer(0)
	if !rt.startMemoryAndAPI(context.Background(), uiServer, nil, &cfg) {
		t.Fatal("initial memory services failed")
	}
	rt.started = true
	t.Cleanup(func() { rt.Stop() })
	oldSrv := rt.apiServer
	if oldSrv == nil {
		t.Fatal("initial API server missing")
	}

	// The applied state says the API is enabled, but the listener is gone: the
	// previous server terminated and its pointer was released, freeing the
	// port.
	if !rt.stopAPIServer(oldSrv) {
		t.Fatal("failed to stop the initial API server")
	}
	rt.apiServer = nil

	// Apply an unchanged config: the listener must be rebuilt and the apply
	// must report it as a live change.
	if !rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil).LiveApplied {
		t.Fatal("missing API listener should be rebuilt and reported as a live apply")
	}
	if rt.apiServer == nil {
		t.Fatal("API server was not rebuilt when the applied state wanted it running")
	}
	if rt.apiServer == oldSrv {
		t.Fatal("rebuilt API server is the stale pointer")
	}
}

// TestAppliedState_PreparedAPIServerNotServedBeforeCommit verifies that a
// prepared API server reserves its port without accepting requests: during
// preparation (before commit) a request to the new port must not be answered,
// the old server keeps serving, and only after commit does the new port serve.
func TestAppliedState_PreparedAPIServerNotServedBeforeCommit(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug
	cfg.API.Enabled = true
	cfg.API.Port = freeTCPPort(t)

	root := initRuntimeProjectRepo(t)
	store := &runtimeConfigStore{cfg: &cfg, saved: true}
	projects := &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
	}}
	rt := New(cfg, store, LogRings{})
	rt.projectStore = projects
	rt.reqQueue = queue.New(4, nil)
	uiServer := ui.NewServer(0)
	if !rt.startMemoryAndAPI(context.Background(), uiServer, nil, &cfg) {
		t.Fatal("initial memory services failed")
	}
	rt.started = true
	t.Cleanup(func() { rt.Stop() })

	prepared := make(chan struct{})
	resume := make(chan struct{})
	rt.afterPrepare = func() {
		close(prepared)
		<-resume
	}

	newPort := freeTCPPort(t)
	for newPort == cfg.API.Port {
		newPort = freeTCPPort(t)
	}
	loaded := cfg
	loaded.API.Port = newPort
	rt.cfgStore = &runtimeConfigStore{cfg: &loaded, saved: true}

	done := make(chan ui.ApplyResult, 1)
	go func() { done <- rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil) }()

	<-prepared
	// During preparation the new port must not be serving: the candidate is
	// bound but not activated, so a request must not be answered.
	client := &http.Client{Timeout: 300 * time.Millisecond}
	if _, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", newPort)); err == nil {
		t.Fatal("prepared-but-uncommitted API server answered a request")
	}
	// The old server keeps serving on the old port during preparation.
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", cfg.API.Port))
	if err != nil {
		t.Fatalf("old API server not serving during preparation: %v", err)
	}
	_ = resp.Body.Close()

	close(resume)
	if res := <-done; !res.LiveApplied {
		t.Fatal("apply did not report live apply")
	}
	// After commit the new port serves.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", newPort))
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("new API server did not serve after commit: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAppliedState_TimeoutShutdownRetainsOwnership verifies that when an API
// server's shutdown does not confirm termination within the timeout, the
// runtime retains ownership of the still-serving server instead of clearing or
// dropping its pointer. The server is tracked for a later Stop that confirms
// termination.
func TestAppliedState_TimeoutShutdownRetainsOwnership(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug
	cfg.API.Enabled = true
	cfg.API.Port = freeTCPPort(t)

	root := initRuntimeProjectRepo(t)
	store := &runtimeConfigStore{cfg: &cfg, saved: true}
	projects := &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
	}}
	rt := New(cfg, store, LogRings{})
	rt.projectStore = projects
	rt.reqQueue = queue.New(4, nil)
	uiServer := ui.NewServer(0)
	if !rt.startMemoryAndAPI(context.Background(), uiServer, nil, &cfg) {
		t.Fatal("initial memory services failed")
	}
	rt.started = true
	t.Cleanup(func() {
		// Restore the real stopper so cleanup actually terminates the servers.
		rt.stopAPIServer = func(s *api.Server) bool { return s.Stop() }
		rt.Stop()
	})

	oldSrv := rt.apiServer
	if oldSrv == nil {
		t.Fatal("initial API server missing")
	}

	// Simulate an API shutdown that never confirms termination.
	rt.stopAPIServer = func(*api.Server) bool { return false }

	newPort := freeTCPPort(t)
	for newPort == cfg.API.Port {
		newPort = freeTCPPort(t)
	}
	loaded := cfg
	loaded.API.Port = newPort
	rt.cfgStore = &runtimeConfigStore{cfg: &loaded, saved: true}

	if !rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil).LiveApplied {
		t.Fatal("API port change should apply live")
	}
	if rt.apiServer == oldSrv {
		t.Fatal("new API server not installed")
	}

	// The old server is still owned by the runtime because termination was
	// never confirmed.
	rt.mu.Lock()
	owned := append([]*api.Server(nil), rt.retiredAPI...)
	rt.mu.Unlock()
	if len(owned) != 1 {
		t.Fatalf("retired API servers = %d, want 1 (ownership must be retained until termination)", len(owned))
	}
	if owned[0] != oldSrv {
		t.Fatal("retired list does not hold the still-running old server")
	}

	// A later Stop that does confirm termination releases ownership.
	rt.stopAPIServer = func(*api.Server) bool { return true }
	rt.drainRetiredAPI()
	rt.mu.Lock()
	owned = append([]*api.Server(nil), rt.retiredAPI...)
	rt.mu.Unlock()
	if len(owned) != 0 {
		t.Fatalf("retired API servers = %d after confirmed termination, want 0", len(owned))
	}
}

// TestAppliedState_ProjectOverrideDeletion verifies that a reload notices the
// deletion of a project model override even when no global config field
// changed. The effective applied model must not remain stale just because the
// global config is identical; the recorded applied state is compared against
// the newly-effective model, so the override deletion retargets the process
// without a generation rebuild.
func TestAppliedState_ProjectOverrideDeletion(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	root := initRuntimeProjectRepo(t)
	binaryB := "project-llama-b"
	modelB := "project-b.gguf"
	projects := &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root,
			ModelBinary: &binaryB, ModelPath: &modelB},
	}}

	rt, projects := appliedRuntimeForTest(t, &cfg, projects)
	oldMgr := rt.SessionManager()
	if rt.applied == nil || rt.applied.runningModel.ModelPath != modelB {
		t.Fatalf("initial applied running model = %+v, want the override model B", rt.applied)
	}
	// The manager must start on the recorded running model (B), so the
	// assertions after deletion prove a real B->A reconfiguration rather than
	// a transition that never happened.
	bin0, args0, _ := rt.llamaMgr.Args()
	if !strings.Contains(strings.Join(args0, " "), modelB) {
		t.Fatalf("initial llama manager not on override model B: %v", args0)
	}
	if bin0 != binaryB {
		t.Fatalf("initial llama binary = %q, want %q", bin0, binaryB)
	}

	// Delete the override: the store now resolves to the global model.
	projects.projects[project.GlobalSlug] = project.Project{
		Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: projects.projects[project.GlobalSlug].MemoryRepoPath,
	}

	// Apply an identical global config. The effective model changed even
	// though no global config field changed, so the apply must notice it.
	loaded := cfg
	rt.cfgStore = &runtimeConfigStore{cfg: &loaded, saved: true}
	uiServer := ui.NewServer(0)
	if !rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil).LiveApplied {
		t.Fatal("override deletion should apply live")
	}

	if rt.applied == nil || rt.applied.runningModel.ModelPath != cfg.Model.ModelPath {
		t.Fatalf("applied running model = %+v, want the global model after override deletion", rt.applied)
	}
	bin, args, _ := rt.llamaMgr.Args()
	if strings.Contains(strings.Join(args, " "), modelB) {
		t.Fatalf("llama still references deleted override model %q: %v", modelB, args)
	}
	if bin != cfg.Model.Binary {
		t.Fatalf("llama binary = %q, want global %q", bin, cfg.Model.Binary)
	}
	// The override deletion is a process-only change: no generation rebuild.
	if rt.SessionManager() != oldMgr {
		t.Fatal("override deletion caused a generation rebuild when only the process needed retargeting")
	}
}

// TestAppliedState_GlobalPortChanges verifies that global model/embedder port
// changes are reflected in the effective applied state and the live process
// configuration without a restart.
func TestAppliedState_GlobalPortChanges(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, _ := appliedRuntimeForTest(t, &cfg, nil)
	newPort := cfg.Model.Port + 1
	newEmbedPort := cfg.Embedder.Port + 1
	loaded := cfg
	loaded.Model.Port = newPort
	loaded.Embedder.Port = newEmbedPort
	rt.cfgStore = &runtimeConfigStore{cfg: &loaded, saved: true}

	uiServer := ui.NewServer(0)
	if !rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil).LiveApplied {
		t.Fatal("port change should apply live")
	}
	if rt.applied == nil {
		t.Fatal("applied state missing")
	}
	if rt.applied.runningModel.Port != newPort {
		t.Fatalf("applied running model port = %d, want %d", rt.applied.runningModel.Port, newPort)
	}
	if rt.applied.runningEmbedder.Port != newEmbedPort {
		t.Fatalf("applied embedder port = %d, want %d", rt.applied.runningEmbedder.Port, newEmbedPort)
	}
	bin, args, _ := rt.llamaMgr.Args()
	if !strings.Contains(strings.Join(args, " "), strconv.Itoa(newPort)) {
		t.Fatalf("llama args %v do not reference new port %d", args, newPort)
	}
	_ = bin
	_, eargs, _ := rt.embedMgr.Args()
	if !strings.Contains(strings.Join(eargs, " "), strconv.Itoa(newEmbedPort)) {
		t.Fatalf("embedder args %v do not reference new port %d", eargs, newEmbedPort)
	}
}

// TestAppliedState_LlamaOnSwitchKeep verifies that project.llama_on_switch=keep
// never reconfigures llama-server during a config apply or project switch, and
// that the actually-running model is recorded separately from the newly
// preferred model so the status UI can represent the mismatch honestly.
func TestAppliedState_LlamaOnSwitchKeep(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug
	cfg.Project.LlamaOnSwitch = "keep"

	rt, projects := appliedRuntimeForTest(t, &cfg, nil)
	modelA := cfg.Model

	// The active project now prefers model B, but keep must leave the running
	// model alone.
	binaryB := "project-llama-b"
	modelB := "project-b.gguf"
	projects.projects[project.GlobalSlug] = project.Project{
		Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: projects.projects[project.GlobalSlug].MemoryRepoPath,
		ModelBinary: &binaryB, ModelPath: &modelB,
	}

	loaded := cfg
	loaded.Prompt.MemoryTokenBudget++
	rt.cfgStore = &runtimeConfigStore{cfg: &loaded, saved: true}

	uiServer := ui.NewServer(0)
	if !rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil).LiveApplied {
		t.Fatal("apply should live-apply prompt/config changes")
	}

	if rt.applied == nil {
		t.Fatal("applied state missing")
	}
	if rt.applied.runningModel.ModelPath != modelA.ModelPath {
		t.Fatalf("running model = %q, want original %q (keep must not reconfigure llama)", rt.applied.runningModel.ModelPath, modelA.ModelPath)
	}
	if rt.applied.model.ModelPath != modelB {
		t.Fatalf("preferred model = %q, want %q (preferred must be recorded separately from running)", rt.applied.model.ModelPath, modelB)
	}
	bin, args, _ := rt.llamaMgr.Args()
	if strings.Contains(strings.Join(args, " "), modelB) || bin == binaryB {
		t.Fatalf("llama was reconfigured to the preferred model despite keep: bin=%q args=%v", bin, args)
	}

	// The status UI must represent the mismatch honestly.
	mismatch, loadedM, preferredM := uiServer.ModelMismatch()
	if !mismatch {
		t.Fatal("expected model mismatch indicator after keep left the running model behind")
	}
	if loadedM != modelA.ModelPath || preferredM != modelB {
		t.Fatalf("mismatch = %q/%q, want %q/%q", loadedM, preferredM, modelA.ModelPath, modelB)
	}
}
