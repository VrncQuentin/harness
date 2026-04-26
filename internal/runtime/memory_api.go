package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/api"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/prompt"
	"github.com/vrnc/harness/internal/ui"
)

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
	rt.agentReg = agent.NewDiskRegistry(rt.memReader, rt.getActiveAgent, rt.setActiveAgent)
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

func (rt *Runtime) getActiveAgent() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.cfg.Agent.Active
}

func (rt *Runtime) setActiveAgent(name string) error {
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
