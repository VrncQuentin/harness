package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/metrics"
	"github.com/VrncQuentin/harness/internal/proc"
	"github.com/VrncQuentin/harness/internal/ui"
)

// ApplyConfig reloads config from the store, validates it, and either starts
// services for the first time or reconfigures the live ones to match. Tier-3
// changes (UI port, queue) are returned as RestartNeeded so the UI can flag
// them - no live apply path exists for those yet.
//
// ApplyConfig is one transaction, serialized end-to-end by applyMu: two
// concurrent applies cannot interleave validation, preparation, process
// changes, generation publication, or retirement. The transaction runs in
// explicit phases:
//
//	prepare  - build the candidate and its API server locally, unpublished
//	quiesce  - cancel task loops and flush sessions before the old generation drops
//	commit   - install the generation and one coherent applied state atomically
//	         - under rt.mu, issuing process reconfigurations from that state
//	retire   - release the old generation's publisher lease and API ownership
//	         - under the timeout ownership protocol
//
// The old/live state is read exclusively from rt.applied (the recorded applied
// state), never reconstructed from the mutable config store or project store.
func (rt *Runtime) ApplyConfig(
	ctx context.Context,
	uiServer *ui.Server,
	events chan proc.Event,
	metricsStore metrics.Store,
) ui.ApplyResult {
	rt.applyMu.Lock()
	defer rt.applyMu.Unlock()
	return rt.applyConfigLocked(ctx, uiServer, events, metricsStore)
}

// applyConfigLocked is the body of ApplyConfig, assuming the caller already
// holds applyMu. The project-edit transaction (EditProject) reuses it so an
// active-project edit's live apply runs inside the same transaction boundary
// as the edit itself, serialized against any concurrent apply or shutdown.
func (rt *Runtime) applyConfigLocked(
	ctx context.Context,
	uiServer *ui.Server,
	events chan proc.Event,
	metricsStore metrics.Store,
) ui.ApplyResult {
	if rt.enterApply != nil {
		rt.enterApply()
	}

	// Install the snapshot provider before anything else so the UI never
	// observes an empty snapshot after a retry-only startup. Start also wires
	// it, but production skips Start on first run, invalid initial config, or
	// failed path validation — a later successful ApplyConfig must still reach
	// handlers with generation-backed deps.
	uiServer.SetSnapshotProvider(rt)

	uiServer.ClearStartupErrors()
	uiServer.SetProjectDirectoryWarnings("", nil)
	if rt.cfgStore == nil {
		uiServer.AddStartupError(ErrConfigStoreUnavailable)
		return ui.ApplyResult{Err: ErrConfigStoreUnavailable}
	}
	loaded, wasSaved, lerr := rt.cfgStore.Load()
	if lerr != nil {
		err := fmt.Errorf("config load: %w", lerr)
		uiServer.AddStartupError(err)
		return ui.ApplyResult{Err: err}
	}
	uiServer.SetFirstRun(!wasSaved)
	if !wasSaved {
		return ui.ApplyResult{}
	}
	if verr := config.Validate(loaded); verr != nil {
		uiServer.AddStartupError(verr)
		return ui.ApplyResult{Err: verr}
	}
	if !ValidatePaths(uiServer, loaded) {
		return ui.ApplyResult{Err: errors.New("path validation failed")}
	}

	rt.mu.Lock()
	oldApplied := rt.applied
	started := rt.started
	oldCfg := rt.cfg
	if oldApplied != nil {
		oldCfg = oldApplied.cfg
	}

	// The preferred model resolves from the loaded config and the current
	// project store. The *old* state comes exclusively from the recorded
	// applied state — never reconstructed from the mutable project store.
	newModel := rt.effectiveModelFor(loaded)
	newEmbedder := loaded.Embedder

	// The model that will actually run. llama_on_switch=keep means a previously
	// running *local* llama-server stays across config applies and project
	// switches; reload retargets it to the new preferred model. The keep
	// semantics apply only when both sides are local — an external endpoint has
	// no process to keep, so the new effective model always becomes the running
	// one. runningModel is recorded separately from model so the status UI can
	// represent a mismatch honestly.
	runningModel := newModel
	if oldApplied != nil && loaded.Project.LlamaOnSwitch == "keep" &&
		oldApplied.runningModel.Kind == config.EndpointKindLocal &&
		newModel.Kind == config.EndpointKindLocal {
		runningModel = oldApplied.runningModel
	}
	newApplied := newAppliedState(loaded, newModel, runningModel)

	projectSwitched := oldApplied != nil &&
		oldApplied.cfg.Project.ActiveProjectSlug != loaded.Project.ActiveProjectSlug
	// modelChanged compares the full running model struct (including Port, which
	// drives the inference client and the llama-server bind): a port change is a
	// process reconfiguration as well as a client repoint. ModelConfigEqual is
	// deliberately not used here — it excludes Port and Verbose for mismatch
	// display, not for apply detection.
	modelChanged := oldApplied != nil && oldApplied.runningModel != runningModel
	embedderChanged := oldApplied != nil && oldApplied.runningEmbedder != newEmbedder
	// kindSwitched marks a local↔external transition. There is no live path for
	// spawning or tearing down the llama-server process mid-run, so a kind
	// switch is recorded but surfaced as a restart-required change.
	kindSwitched := oldApplied != nil && oldApplied.runningModel.Kind != runningModel.Kind
	// backendChanged marks an external-endpoint identity change (base url, api
	// key, or model id) that needs a live inference-client repoint rather than
	// a process reconfiguration. Local↔local changes are process reconfigures.
	backendChanged := oldApplied != nil && !kindSwitched && needsClientRepoint(oldApplied.runningModel, runningModel)

	// A local↔external kind switch has no live path: the llama-server process
	// cannot be spawned or torn down mid-run, and the inference client cannot
	// be repointed without leaving the recorded applied state out of step with
	// the live process. The config is already persisted by the caller, so the
	// switch takes effect on the next harness restart. The live applied state,
	// processes, and client are left untouched.
	if kindSwitched {
		slog.Info("model backend switch requires a restart",
			"old_kind", oldApplied.runningModel.Kind, "new_kind", runningModel.Kind,
			"new_endpoint", runningModel.EndpointID, "new_model", runningModel.ModelID)
		result := ui.ApplyResult{RestartNeeded: []string{"model backend"}}
		rt.finishResult(&result, oldCfg, loaded)
		rt.mu.Unlock()
		return result
	}

	rebuild := rt.memoryAPIUnavailable()
	if oldApplied != nil {
		rebuild = rebuild ||
			oldApplied.cfg.Prompt != loaded.Prompt ||
			oldApplied.cfg.API != loaded.API ||
			oldApplied.cfg.Loop != loaded.Loop ||
			oldApplied.cfg.Agent.Active != loaded.Agent.Active ||
			projectSwitched ||
			backendChanged ||
			oldApplied.runningModel.CtxSize != runningModel.CtxSize ||
			oldApplied.embedder.Port != newEmbedder.Port
	}
	apiPortChanged := apiPortNeedsChange(oldApplied, loaded)
	// The listener build decision is live-aware: rebuild can be forced by
	// memoryAPIUnavailable finding rt.apiServer == nil while the applied config
	// wants the API running, so the candidate must rebuild the listener even
	// when the recorded config is unchanged.
	buildAPI := apiServerNeedsBuild(oldApplied, loaded, rt.apiServer != nil)

	if !started {
		slog.Info("starting services", "model_kind", newModel.Kind, "endpoint", newModel.EndpointID, "model", newModel.ModelID, "model_port", newModel.Port, "embed_port", newEmbedder.Port)
		rt.cfg = *loaded
		rt.refreshProjectDirectoryWarnings(uiServer)
		rt.startServices(ctx, uiServer, events, metricsStore)
		ok := rt.startMemoryAndAPI(ctx, uiServer, metricsStore, loaded)
		var result ui.ApplyResult
		if ok {
			result.LiveApplied = true
		} else {
			result.Err = errors.New("memory/API services failed to start")
		}
		rt.finishResult(&result, oldCfg, loaded)
		rt.mu.Unlock()
		rt.drainPendingRetired()
		rt.setModelMismatch(uiServer, rt.applied)
		if rt.leaveApply != nil {
			rt.leaveApply()
		}
		return result
	}
	rt.mu.Unlock()

	// PREPARE: build the candidate (and its API server when the port/enabled
	// state changed) locally. Nothing is published and no process is touched
	// until commit; a failed candidate is discarded wholesale and the
	// installed generation and recorded applied state stay as they were.
	var candidate *memoryCandidate
	if rebuild {
		candidate = rt.prepareApply(ctx, uiServer, metricsStore, loaded, runningModel, buildAPI)
		if candidate == nil {
			if rt.leaveApply != nil {
				rt.leaveApply()
			}
			return ui.ApplyResult{Err: errors.New("candidate preparation failed")}
		}
	}
	if rt.afterPrepare != nil {
		rt.afterPrepare()
	}

	// QUIESCE + COMMIT: cancel task loops and flush sessions before the old
	// generation is dropped, then install the candidate and applied state
	// atomically and retire the previous resources. quiesce releases rt.mu
	// while it waits so session summarization can read live config without
	// deadlocking; commit reacquires it.
	rt.mu.Lock()
	if rebuild {
		rt.quiesceMemoryAndAPI(ctx)
	}
	drainQueue := projectSwitched && modelChanged && rt.reqQueue != nil
	rt.mu.Unlock()

	// Draining the request queue on a project-switch model reload happens
	// outside rt.mu: it waits for in-flight requests that would otherwise be
	// dispatched to a llama-server that is about to be killed.
	if drainQueue {
		if err := rt.reqQueue.Restart(ctx); err != nil {
			slog.Warn("project switch: queue restart failed", "err", err)
		}
	}

	rt.mu.Lock()
	result := rt.commitApply(candidate, &newApplied, oldApplied, modelChanged, embedderChanged, backendChanged, apiPortChanged, oldCfg, uiServer)
	rt.mu.Unlock()

	// Retirement of the previous API server runs outside rt.mu: Stop can wait
	// on active connections. A server that outlives its shutdown timeout keeps
	// a retained slot (see drainPendingRetired).
	rt.drainPendingRetired()

	if rt.leaveApply != nil {
		rt.leaveApply()
	}
	rt.setModelMismatch(uiServer, &newApplied)
	return result
}

// apiPortNeedsChange reports whether the API listener must be rebuilt when
// applying loaded. The comparison is against the recorded applied state, so an
// enabled/disabled or port change is noticed even when the rest of the config
// is identical.
func apiPortNeedsChange(old *appliedState, loaded *config.Config) bool {
	wasRunning := old != nil && old.cfg.API.Enabled
	wantRunning := loaded.API.Enabled
	if wasRunning != wantRunning {
		return true
	}
	return wasRunning && old.cfg.API.Port != loaded.API.Port
}

// apiServerNeedsBuild reports whether the apply must build a fresh API
// listener. It covers both a config-driven enabled/port change and an
// enabled-but-missing listener: rebuild can be forced by memoryAPIUnavailable
// finding rt.apiServer == nil while the applied config wants the API running,
// so the build decision cannot rely on config diffs alone.
func apiServerNeedsBuild(old *appliedState, loaded *config.Config, apiRunning bool) bool {
	if !loaded.API.Enabled {
		return false
	}
	return apiPortNeedsChange(old, loaded) || !apiRunning
}

// finishResult appends restart-required reasons and live-applies the log-ring
// resizes, mirroring the apply tail. Caller must hold rt.mu.
func (rt *Runtime) finishResult(result *ui.ApplyResult, oldCfg config.Config, newCfg *config.Config) {
	if oldCfg.UI.Port != newCfg.UI.Port {
		result.RestartNeeded = append(result.RestartNeeded, "UI port")
	}
	if oldCfg.Queue.MaxDepth != newCfg.Queue.MaxDepth {
		result.RestartNeeded = append(result.RestartNeeded, "queue max depth")
	}
	// Metrics retention is read dynamically from rt.cfg by the running metrics
	// loop, so committing a new retention is a live change even though no
	// service is rebuilt.
	if oldCfg.Metrics.RetentionDays != newCfg.Metrics.RetentionDays {
		result.LiveApplied = true
	}
	if oldCfg.Log.RingMaxEntries != newCfg.Log.RingMaxEntries && rt.logRings.Log != nil {
		rt.logRings.Log.Resize(newCfg.Log.RingMaxEntries)
		result.LiveApplied = true
	}
	if oldCfg.Log.ProcMaxLines != newCfg.Log.ProcMaxLines {
		if rt.logRings.Llama != nil {
			rt.logRings.Llama.Resize(newCfg.Log.ProcMaxLines)
		}
		if rt.logRings.Embed != nil {
			rt.logRings.Embed.Resize(newCfg.Log.ProcMaxLines)
		}
		result.LiveApplied = true
	}
}

// needsClientRepoint reports whether applying the new running model requires
// repointing the inference client rather than reconfiguring the llama-server
// process. Local↔local changes are process reconfigures, except a port change
// which also moves the client's base URL; a local↔external kind switch is
// handled as a restart-required change elsewhere, not a live repoint.
func needsClientRepoint(old, new config.ModelConfig) bool {
	if old.Kind == config.EndpointKindLocal && new.Kind == config.EndpointKindLocal {
		return old.Port != new.Port
	}
	if old.Kind != new.Kind {
		return false
	}
	return old.BaseURL != new.BaseURL ||
		old.APIKey != new.APIKey ||
		old.ModelID != new.ModelID
}

// setModelMismatch reflects the recorded running-versus-preferred model on the
// status UI. The two are recorded separately (see appliedState) so the UI can
// represent llama_on_switch=keep honestly. Mismatch only applies to a local
// llama-server: an external endpoint has no process to lag, so it always reads
// as consistent.
func (rt *Runtime) setModelMismatch(uiServer *ui.Server, applied *appliedState) {
	if uiServer == nil || applied == nil {
		return
	}
	if applied.runningModel.Kind == config.EndpointKindOpenAI ||
		applied.model.Kind == config.EndpointKindOpenAI {
		uiServer.SetModelMismatch(false, "", "")
		return
	}
	if !config.ModelConfigEqual(applied.runningModel, applied.model) {
		uiServer.SetModelMismatch(true, applied.runningModel.ModelPath, applied.model.ModelPath)
		return
	}
	uiServer.SetModelMismatch(false, "", "")
}
