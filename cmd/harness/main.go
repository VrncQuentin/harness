// Command harness is the main entry point for the local AI inference harness.
// It follows the startup sequence defined in docs/architecture.md.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // register the sqlite driver

	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/metrics"
	"github.com/vrnc/harness/internal/proc"
	"github.com/vrnc/harness/internal/queue"
	"github.com/vrnc/harness/internal/tray"
	"github.com/vrnc/harness/internal/ui"
	"github.com/vrnc/harness/pkg/httpclient"
)

const dbFilename = "harness.db"

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

	// Step 2: Resolve binary directory for the shared DB, WAL, etc.
	binDir, err := binaryDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness: cannot determine binary dir: %v\n", err)
		os.Exit(1)
	}

	// Root context - cancelled when tray quit is triggered.
	rootCtx, rootCancel := context.WithCancel(context.Background())

	// Step 3: Start UI server - must succeed before we proceed.
	uiServer := ui.NewServer(3000)
	if err := uiServer.Start(rootCtx); err != nil {
		fmt.Fprintf(os.Stderr, "harness: UI server failed to start: %v\n", err)
		os.Exit(1)
	}

	// Step 4: Open the shared harness database.
	dbPath := filepath.Join(binDir, dbFilename)
	db, cfgStore, metricsStore := openStores(uiServer, dbPath)

	// Step 5: Load config (or fall back to defaults if store is unavailable).
	cfg := config.Defaults()
	configured := false
	if cfgStore != nil {
		loaded, wasSaved, lerr := cfgStore.Load()
		if lerr != nil {
			uiServer.AddStartupError(fmt.Errorf("config load: %w", lerr))
		} else {
			cfg = *loaded
			configured = wasSaved
		}
	}
	uiServer.SetFirstRun(!configured)

	uiServer.SetRetry(func() {
		uiServer.ClearStartupErrors()
		if cfgStore == nil {
			uiServer.AddStartupError(fmt.Errorf("config store unavailable (harness.db could not be opened)"))
			return
		}
		loaded, wasSaved, lerr := cfgStore.Load()
		if lerr != nil {
			uiServer.AddStartupError(fmt.Errorf("config load: %w", lerr))
			return
		}
		uiServer.SetFirstRun(!wasSaved)
		if !wasSaved {
			return
		}
		if verr := config.Validate(loaded); verr != nil {
			uiServer.AddStartupError(verr)
			return
		}
		validatePaths(uiServer, loaded)
	})

	// Open browser to UI unless disabled by saved config.
	if cfg.UI.OpenOnStart {
		go func() {
			time.Sleep(200 * time.Millisecond)
			exec.Command("cmd", "/c", "start", "http://localhost:3000").Run() //nolint:errcheck
		}()
	}

	// Event channel for process manager log events.
	events := make(chan proc.Event, 64)

	var llamaMgr, embedMgr *proc.Manager
	var reqQueue *queue.Queue

	if configured {
		validatePaths(uiServer, &cfg)

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

		// Build inference client and queue.
		inferClient := inference.NewClient(
			fmt.Sprintf("http://127.0.0.1:%d", cfg.Model.Port),
			httpclient.NewStreaming(),
		)
		reqQueue = queue.New(cfg.Queue.MaxDepth, cfg.Queue.WALPath, inferClient)
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
		if db != nil {
			_ = db.Close()
		}
	}

	// Step 9: Hand off to tray.Run() - this blocks until Quit.
	uiURL := fmt.Sprintf("http://localhost:%d", cfg.UI.Port)
	tray.Run(uiURL, onQuit)
}

// openStores opens harness.db and initializes the config + metrics stores on
// top of it. Any failure is surfaced to the UI as a startup error; the
// returned stores may be nil, which callers must handle.
func openStores(uiServer *ui.Server, path string) (*sql.DB, *config.Store, metrics.Store) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("harness.db open: %w", err))
		return nil, nil, nil
	}
	if err := db.Ping(); err != nil {
		uiServer.AddStartupError(fmt.Errorf("harness.db ping: %w", err))
		_ = db.Close()
		return nil, nil, nil
	}

	cfgStore, err := config.Open(db)
	if err != nil {
		uiServer.AddStartupError(err)
		_ = db.Close()
		return nil, nil, nil
	}
	uiServer.SetConfigStore(cfgStore)

	metricsStore, err := metrics.Open(db)
	if err != nil {
		uiServer.AddStartupError(err)
		// Config store is still usable; keep the DB open.
		return db, cfgStore, nil
	}
	return db, cfgStore, metricsStore
}

// validatePaths checks that the binaries and model files referenced by cfg
// exist on disk and surfaces any missing ones as startup errors.
func validatePaths(uiServer *ui.Server, cfg *config.Config) {
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
		if _, err := os.Stat(c.path); os.IsNotExist(err) {
			uiServer.AddStartupError(fmt.Errorf("%s not found: %s", c.label, c.path))
		}
	}
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
			if q != nil {
				store.Record("queue_depth", float64(q.Depth()), nil) //nolint:errcheck
			}

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
