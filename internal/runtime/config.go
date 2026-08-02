package runtime

import (
	"context"
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
	if !ValidatePaths(uiServer, loaded) {
		return ui.ApplyResult{}
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

	// The model that will actually run. llama_on_switch=keep means the
	// previously running model stays across config applies and project
	// switches; reload retargets it to the new preferred model. runningModel
	// is recorded separately from model so the status UI can represent a
	// mismatch honestly.
	runningModel := newModel
	if oldApplied != nil && loaded.Project.LlamaOnSwitch == "keep" {
		runningModel = oldApplied.runningModel
	}
	newApplied := newAppliedState(loaded, newModel, runningModel)

	projectSwitched := oldApplied != nil &&
		oldApplied.cfg.Project.ActiveProjectSlug != loaded.Project.ActiveProjectSlug
	modelChanged := oldApplied != nil && oldApplied.runningModel != runningModel
	embedderChanged := oldApplied != nil && oldApplied.runningEmbedder != newEmbedder
	endpointChanged := oldApplied != nil && oldApplied.runningModel.Port != runningModel.Port

	rebuild := rt.memoryAPIUnavailable()
	if oldApplied != nil {
		rebuild = rebuild ||
			oldApplied.cfg.Prompt != loaded.Prompt ||
			oldApplied.cfg.API != loaded.API ||
			oldApplied.cfg.Loop != loaded.Loop ||
			oldApplied.cfg.Agent.Active != loaded.Agent.Active ||
			projectSwitched ||
			endpointChanged ||
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
		slog.Info("starting services", "model_port", newModel.Port, "embed_port", newEmbedder.Port)
		rt.cfg = *loaded
		rt.refreshProjectDirectoryWarnings(uiServer)
		rt.startServices(ctx, uiServer, events, metricsStore)
		ok := rt.startMemoryAndAPI(ctx, uiServer, metricsStore, loaded)
		var result ui.ApplyResult
		if ok {
			result.LiveApplied = true
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
	var tx *applyTx
	if rebuild {
		tx = rt.prepareApply(ctx, uiServer, metricsStore, loaded, runningModel, buildAPI)
		if tx == nil {
			if rt.leaveApply != nil {
				rt.leaveApply()
			}
			return ui.ApplyResult{}
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
	result := rt.commitApply(tx, &newApplied, oldApplied, modelChanged, embedderChanged, endpointChanged, apiPortChanged, oldCfg, uiServer)
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

// setModelMismatch reflects the recorded running-versus-preferred model on the
// status UI. The two are recorded separately (see appliedState) so the UI can
// represent llama_on_switch=keep honestly.
func (rt *Runtime) setModelMismatch(uiServer *ui.Server, applied *appliedState) {
	if uiServer == nil || applied == nil {
		return
	}
	if !config.ModelConfigEqual(applied.runningModel, applied.model) {
		uiServer.SetModelMismatch(true, applied.runningModel.ModelPath, applied.model.ModelPath)
		return
	}
	uiServer.SetModelMismatch(false, "", "")
}
