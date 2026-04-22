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
	"github.com/vrnc/harness/pkg/httpclient"
)

func main() {
	// Step 1: Acquire single-instance mutex (Windows only).
	first, err := tray.AcquireSingleInstance()
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness: single-instance check failed: %v\n", err)
		os.Exit(1)
	}
	if !first {
		os.Exit(0)
	}

	// Step 2: Resolve binary directory for config, metrics, etc.
	binDir, err := binaryDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness: cannot determine binary dir: %v\n", err)
		os.Exit(1)
	}

	// Root context - cancelled when tray quit is triggered.
	rootCtx, rootCancel := context.WithCancel(context.Background())

	// Step 3: Start UI server - must succeed before we proceed.
	uiServer := ui.NewServer(3000, binDir)
	if err := uiServer.Start(rootCtx); err != nil {
		fmt.Fprintf(os.Stderr, "harness: UI server failed to start: %v\n", err)
		os.Exit(1)
	}

	// Step 4: Open browser if configured (default true until config says otherwise).
	openBrowser := true

	// Step 5: Load config. validateStartup re-runs this whenever Retry is clicked
	// or the config editor saves a new file. Process managers still bind to the
	// initial config for this binary run; model/embedder/port changes require
	// restarting the harness.
	cfg, cfgErr := config.Load(binDir)
	if cfgErr != nil {
		uiServer.AddStartupError(fmt.Errorf("config error: %w (expected at %s)", cfgErr, config.ConfigPath(binDir)))
	}

	uiServer.SetRetry(func() {
		uiServer.ClearStartupErrors()
		newCfg, err := config.Load(binDir)
		if err != nil {
			uiServer.AddStartupError(fmt.Errorf("config error: %w (expected at %s)", err, config.ConfigPath(binDir)))
			return
		}
		if _, err := os.Stat(newCfg.Model.ModelPath); os.IsNotExist(err) {
			uiServer.AddStartupError(fmt.Errorf("model file not found: %s", newCfg.Model.ModelPath))
		}
		if _, err := os.Stat(newCfg.Model.Binary); os.IsNotExist(err) {
			uiServer.AddStartupError(fmt.Errorf("llama-server binary not found: %s", newCfg.Model.Binary))
		}
		if _, err := os.Stat(newCfg.Embedder.Binary); os.IsNotExist(err) {
			uiServer.AddStartupError(fmt.Errorf("embedder binary not found: %s", newCfg.Embedder.Binary))
		}
		if _, err := os.Stat(newCfg.Embedder.ModelPath); os.IsNotExist(err) {
			uiServer.AddStartupError(fmt.Errorf("embedder model file not found: %s", newCfg.Embedder.ModelPath))
		}
	})

	if cfg != nil {
		openBrowser = cfg.UI.OpenOnStart
	}

	if openBrowser {
		go func() {
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
		// Validate model file and binary exist.
		if _, err := os.Stat(cfg.Model.ModelPath); os.IsNotExist(err) {
			uiServer.AddStartupError(fmt.Errorf("model file not found: %s", cfg.Model.ModelPath))
		}
		if _, err := os.Stat(cfg.Model.Binary); os.IsNotExist(err) {
			uiServer.AddStartupError(fmt.Errorf("llama-server binary not found: %s", cfg.Model.Binary))
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
					cfg.Model.Port,
				)
			},
			HealthURL:   fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Model.Port),
			Events:      events,
			CheckPeriod: 5 * time.Second,
			HTTPClient:  httpclient.New(),
		})
		go llamaMgr.Run(rootCtx)

		// Step 7: Start process manager for embedder sidecar.
		embedMgr = proc.NewManager(proc.ManagerConfig{
			Name: "embedder",
			BuildArgs: func() (string, []string) {
				return proc.EmbedderArgs(
					cfg.Embedder.Binary,
					cfg.Embedder.ModelPath,
					cfg.Embedder.Port,
				)
			},
			HealthURL:   fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Embedder.Port),
			Events:      events,
			CheckPeriod: 5 * time.Second,
			HTTPClient:  httpclient.New(),
		})
		go embedMgr.Run(rootCtx)

		// Open metrics store.
		dbPath := cfg.Metrics.DBPath
		if !filepath.IsAbs(dbPath) {
			dbPath = filepath.Join(binDir, dbPath)
		}
		ms, err := metrics.Open(dbPath)
		if err != nil {
			uiServer.AddStartupError(fmt.Errorf("metrics DB error: %w", err))
		} else {
			metricsStore = ms
		}

		// Build inference client and queue.
		inferClient := inference.NewClient(
			fmt.Sprintf("http://127.0.0.1:%d", cfg.Model.Port),
			httpclient.NewStreaming(),
		)
		walPath := cfg.Queue.WALPath
		reqQueue = queue.New(cfg.Queue.MaxDepth, walPath, inferClient)
		if err := reqQueue.Start(rootCtx); err != nil {
			uiServer.AddStartupError(fmt.Errorf("queue WAL error: %w", err))
		}

		// Step 8: Metrics recording goroutine.
		if metricsStore != nil {
			go recordMetrics(rootCtx, metricsStore, llamaMgr, embedMgr, reqQueue)
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

	// Step 9: Hand off to tray.Run() - this blocks until Quit.
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
) {
	start := time.Now()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			store.Record("uptime_seconds", time.Since(start).Seconds(), nil) //nolint:errcheck
			store.Record("queue_depth", float64(q.Depth()), nil)             //nolint:errcheck

			if llamaMgr != nil {
				st := llamaMgr.Status()
				h := 0.0
				if st.Healthy {
					h = 1.0
				}
				store.Record("process_health", h, map[string]string{"process": "llama-server"})                       //nolint:errcheck
				store.Record("restart_count", float64(st.RestartCount), map[string]string{"process": "llama-server"}) //nolint:errcheck
			}
			if embedMgr != nil {
				st := embedMgr.Status()
				h := 0.0
				if st.Healthy {
					h = 1.0
				}
				store.Record("process_health", h, map[string]string{"process": "embedder"})                       //nolint:errcheck
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
