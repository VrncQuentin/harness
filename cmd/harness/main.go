// Command harness is the main entry point for the local AI inference harness.
// It follows the startup sequence defined in docs/architecture.md.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/metrics"
	"github.com/vrnc/harness/internal/proc"
	"github.com/vrnc/harness/internal/queue"
	"github.com/vrnc/harness/internal/tray"
	"github.com/vrnc/harness/internal/ui"
)

func main() {
	// Step 1: Acquire single-instance mutex (Windows only).
	// If another instance is already running, exit silently.
	first, err := tray.AcquireSingleInstance()
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness: single-instance check failed: %v\n", err)
		os.Exit(1)
	}
	if !first {
		// Second instance — exit silently.
		os.Exit(0)
	}

	// Step 2: Resolve binary directory for config, metrics, etc.
	binDir, err := binaryDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness: cannot determine binary dir: %v\n", err)
		os.Exit(1)
	}

	// Root context — cancelled when tray quit is triggered.
	rootCtx, rootCancel := context.WithCancel(context.Background())

	// Step 3: Start UI server — always succeeds, runs before config is loaded.
	uiServer := ui.NewServer(3000)
	if err := uiServer.Start(rootCtx); err != nil {
		// Should never happen, but if it does, still continue.
		fmt.Fprintf(os.Stderr, "harness: UI server error: %v\n", err)
	}

	// Step 4: Open browser if configured (we do this after UI is up).
	// We attempt it regardless of config; config may disable it.
	openBrowser := true // default until we load config

	// Step 5: Load config.
	cfg, cfgErr := config.Load(binDir)
	if cfgErr != nil {
		uiServer.AddStartupError(fmt.Sprintf("Config error: %s (expected at %s)",
			cfgErr.Error(), config.ConfigPath(binDir)))
	}

	if cfg != nil && cfg.UI.OpenOnStart {
		openBrowser = true
	} else if cfg != nil {
		openBrowser = cfg.UI.OpenOnStart
	}

	if openBrowser {
		go func() {
			// Small delay to let the server bind before opening the browser.
			time.Sleep(200 * time.Millisecond)
			exec.Command("cmd", "/c", "start", "http://localhost:3000").Run() //nolint:errcheck
		}()
	}

	// Event channel for process manager log events.
	events := make(chan proc.Event, 64)

	var llamaMgr, embedMgr *proc.Manager
	var metricsStore metrics.Store
	var reqQueue *queue.Queue

	if cfg != nil {
		// Validate model file exists.
		if _, err := os.Stat(cfg.Model.ModelPath); os.IsNotExist(err) {
			uiServer.AddStartupError(fmt.Sprintf(
				"Model file not found: %s", cfg.Model.ModelPath))
		}

		// Validate llama-server binary.
		if _, err := os.Stat(cfg.Model.Binary); os.IsNotExist(err) {
			uiServer.AddStartupError(fmt.Sprintf(
				"llama-server binary not found: %s", cfg.Model.Binary))
		}

		// Step 6: Start process manager for llama-server.
		llamaMgr = proc.NewManager(proc.ManagerConfig{
			Name: "llama-server",
			BuildArgs: func() (string, []string) {
				return proc.LlamaArgs(
					cfg.Model.Binary,
					cfg.Model.ModelPath,
					cfg.Model.CtxSize,
					cfg.Model.GPULayers,
					cfg.Model.NParallel,
				)
			},
			HealthURL:   "http://127.0.0.1:8081/health",
			Events:      events,
			CheckPeriod: 5 * time.Second,
		})
		llamaMgr.Start(rootCtx)

		// Step 7: Start process manager for embedder sidecar.
		embedMgr = proc.NewManager(proc.ManagerConfig{
			Name: "embedder",
			BuildArgs: func() (string, []string) {
				return proc.EmbedderArgs(
					cfg.Embedder.Binary,
					cfg.Embedder.ModelPath,
				)
			},
			HealthURL:   "http://127.0.0.1:8082/health",
			Events:      events,
			CheckPeriod: 5 * time.Second,
		})
		embedMgr.Start(rootCtx)

		// Open metrics store.
		dbPath := cfg.Metrics.DBPath
		if !filepath.IsAbs(dbPath) {
			dbPath = filepath.Join(binDir, dbPath)
		}
		ms, err := metrics.Open(dbPath)
		if err != nil {
			uiServer.AddStartupError(fmt.Sprintf("Metrics DB error: %v", err))
		} else {
			metricsStore = ms
		}

		// Build inference client and queue.
		inferClient := inference.NewClient("http://127.0.0.1:8081")
		walPath := cfg.Queue.WALPath
		reqQueue = queue.New(cfg.Queue.MaxDepth, walPath, inferClient)
		if err := reqQueue.Start(rootCtx); err != nil {
			uiServer.AddStartupError(fmt.Sprintf("Queue WAL error: %v", err))
		}

		// Step 8: Metrics recording goroutine.
		if metricsStore != nil {
			go recordMetrics(rootCtx, metricsStore, llamaMgr, embedMgr, reqQueue, cfg.Queue.MaxDepth)
		}
	}

	// Forward process events to UI.
	go forwardEvents(rootCtx, events, uiServer, llamaMgr, embedMgr)

	// Shutdown function called by tray Quit.
	onQuit := func() {
		rootCancel()
		if reqQueue != nil {
			reqQueue.Stop()
		}
		if metricsStore != nil {
			metricsStore.Close() //nolint:errcheck
		}
	}

	// Step 9: Hand off to tray.Run() — this blocks until Quit.
	uiURL := "http://localhost:3000"
	if cfg != nil {
		uiURL = fmt.Sprintf("http://localhost:%d", cfg.UI.Port)
	}
	tray.Run(uiURL, onQuit)
}

// binaryDir returns the directory containing the running binary.
func binaryDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("os.Executable: %w", err)
	}
	return filepath.Dir(exe), nil
}

// recordMetrics periodically writes process and queue metrics to the store.
func recordMetrics(
	ctx context.Context,
	store metrics.Store,
	llamaMgr, embedMgr *proc.Manager,
	q *queue.Queue,
	queueMax int,
) {
	start := time.Now()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			uptime := time.Since(start).Seconds()
			store.Record("uptime_seconds", uptime, nil)                    //nolint:errcheck
			store.Record("queue_depth", float64(q.Depth()), nil)           //nolint:errcheck

			if llamaMgr != nil {
				st := llamaMgr.Status()
				health := 0.0
				if st.Healthy {
					health = 1.0
				}
				store.Record("process_health", health, map[string]string{"process": "llama-server"}) //nolint:errcheck
				store.Record("restart_count", float64(st.RestartCount), map[string]string{"process": "llama-server"}) //nolint:errcheck
			}
			if embedMgr != nil {
				st := embedMgr.Status()
				health := 0.0
				if st.Healthy {
					health = 1.0
				}
				store.Record("process_health", health, map[string]string{"process": "embedder"}) //nolint:errcheck
				store.Record("restart_count", float64(st.RestartCount), map[string]string{"process": "embedder"}) //nolint:errcheck
			}
		}
	}
}

// forwardEvents reads process events and updates the UI state.
func forwardEvents(
	ctx context.Context,
	events <-chan proc.Event,
	uiSrv *ui.Server,
	llamaMgr, embedMgr *proc.Manager,
) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-events:
			if !ok {
				return
			}
			// Update UI from current manager state after any event.
			pushStatus(uiSrv, llamaMgr, embedMgr)
		case <-ticker.C:
			pushStatus(uiSrv, llamaMgr, embedMgr)
		}
	}
}

// pushStatus reads current manager states and pushes them to the UI.
func pushStatus(uiSrv *ui.Server, llamaMgr, embedMgr *proc.Manager) {
	if llamaMgr != nil {
		st := llamaMgr.Status()
		uiSrv.SetLlamaStatus(ui.ProcessStatus{
			Name:         "llama-server",
			Running:      st.Running,
			Healthy:      st.Healthy,
			RestartCount: st.RestartCount,
			LastError:    st.LastError,
		})
	}
	if embedMgr != nil {
		st := embedMgr.Status()
		uiSrv.SetEmbedderStatus(ui.ProcessStatus{
			Name:         "embedder",
			Running:      st.Running,
			Healthy:      st.Healthy,
			RestartCount: st.RestartCount,
			LastError:    st.LastError,
		})
	}
}
