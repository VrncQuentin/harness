// Command harness is the main entry point for the local AI inference harness.
// It follows the startup sequence defined in docs/architecture.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/db"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/logbuf"
	"github.com/vrnc/harness/internal/metrics"
	"github.com/vrnc/harness/internal/proc"
	"github.com/vrnc/harness/internal/queue"
	"github.com/vrnc/harness/internal/tray"
	"github.com/vrnc/harness/internal/ui"
	"github.com/vrnc/harness/pkg/httpclient"
)

// errConfigStoreUnavailable is surfaced when the harness DB could not be
// opened, so the user sees one consistent message in the status page and the
// config editor.
var errConfigStoreUnavailable = errors.New("config store unavailable (harness.db could not be opened)")

const dbFilename = "harness.db"

// runtime holds mutable service references that the retry/save callback
// reconfigures in place. A mutex guards all fields because the callback runs
// on an HTTP goroutine while other goroutines (forwardEvents, metrics) read
// the same managers and queue.
type runtime struct {
	mu       sync.Mutex
	cfg      config.Config
	logRing  *logbuf.Ring
	llamaMgr *proc.Manager
	embedMgr *proc.Manager
	reqQueue *queue.Queue
	started  bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		os.Exit(1)
	}
}

// run wires up the harness and blocks until the tray Quit menu is selected.
// Errors returned here are fatal; non-fatal startup errors are surfaced to the
// UI instead.
func run() error {
	// Step 1: Acquire single-instance mutex (Windows only).
	first, err := tray.AcquireSingleInstance()
	if err != nil {
		return fmt.Errorf("single-instance check: %w", err)
	}
	if !first {
		return nil
	}

	// Step 2: Resolve binary directory for the shared DB, WAL, etc.
	binDir, err := binaryDir()
	if err != nil {
		return fmt.Errorf("cannot determine binary dir: %w", err)
	}

	// Redirect os.Stdout to os.Stderr so any stray fmt.Println or direct
	// Stdout writes (ours or from dependencies) flow through the same sink
	// as slog/log below, instead of escaping to the void.
	os.Stdout = os.Stderr

	// Tee the default log + slog outputs into an in-memory ring so the
	// status page can show recent harness output. Stderr still receives
	// everything so terminal launches are unchanged. The ring is sized from
	// the default config because we haven't opened the DB yet - saved values
	// take effect on the next harness launch, like UI port.
	//
	// We cannot use io.MultiWriter here: in a `-H windowsgui` build there is
	// no attached console, so os.Stderr.Write fails, and MultiWriter returns
	// the error without writing to later writers -- meaning the ring stays
	// empty and the log panel never populates. tee writes to each sink and
	// swallows per-sink errors so one bad writer can't silence the others.
	logRing := logbuf.New(config.Defaults().Log.RingMaxEntries)
	logSink := tee(os.Stderr, logRing)
	log.SetOutput(logSink)
	slog.SetDefault(slog.New(slog.NewTextHandler(logSink, nil)))

	slog.Info("harness starting", "binDir", binDir)

	// Root context - cancelled when tray quit is triggered.
	rootCtx, rootCancel := context.WithCancel(context.Background())

	// Step 3: Start UI server - must succeed before we proceed.
	uiServer := ui.NewServer(3000)
	uiServer.SetBinDir(binDir)
	uiServer.SetLogRing(logRing)
	if err := uiServer.Start(rootCtx); err != nil {
		rootCancel()
		return fmt.Errorf("UI server start: %w", err)
	}
	slog.Info("ui server listening", "url", "http://localhost:3000")

	// Step 4: Open the shared harness database.
	dbPath := filepath.Join(binDir, dbFilename)
	harnessDB, cfgStore, metricsStore := openDB(uiServer, dbPath)
	if harnessDB != nil {
		slog.Info("harness.db opened", "path", dbPath)
	}

	// Step 5: Load config (or fall back to defaults if store is unavailable).
	cfg := config.Defaults()
	configured := false
	if cfgStore != nil {
		loaded, wasSaved, lerr := cfgStore.Load()
		if lerr != nil {
			slog.Error("config load failed", "err", lerr)
			uiServer.AddStartupError(fmt.Errorf("config load: %w", lerr))
		} else {
			cfg = *loaded
			configured = wasSaved
		}
	}
	uiServer.SetFirstRun(!configured)
	if configured {
		slog.Info("config loaded", "model_port", cfg.Model.Port, "embed_port", cfg.Embedder.Port)
	} else {
		slog.Info("first run: waiting for config")
	}

	// proc.Manager.emit is non-blocking and drops on a full buffer; size 64
	// is large enough to absorb startup bursts (multiple managers emitting
	// start/health events back-to-back) without losing them.
	events := make(chan proc.Event, 64)
	rt := &runtime{cfg: cfg, logRing: logRing}

	// Boot-time start: if the user has previously saved config, bring services
	// up right away. Otherwise they will be created on the first /config save.
	if configured {
		validatePaths(uiServer, &cfg)
		rt.mu.Lock()
		rt.startServices(rootCtx, uiServer, events, metricsStore)
		rt.mu.Unlock()
	}

	// forwardEvents needs to see managers that may be created later (first-run
	// save path), so it fetches them via a getter under rt.mu.
	go forwardEvents(rootCtx, events, uiServer, rt.getManagers)

	uiServer.SetRetry(func() ui.ApplyResult {
		return rt.applyConfig(rootCtx, uiServer, cfgStore, events, metricsStore)
	})

	// Open browser to UI unless disabled by saved config.
	if cfg.UI.OpenOnStart {
		go func() {
			time.Sleep(200 * time.Millisecond)
			exec.Command("cmd", "/c", "start", "http://localhost:3000").Run() //nolint:errcheck
		}()
	}

	// Shutdown function called by tray Quit.
	onQuit := func() {
		slog.Info("harness shutting down")
		rootCancel()
		rt.mu.Lock()
		q := rt.reqQueue
		rt.mu.Unlock()
		if q != nil {
			q.Stop()
		}
		if harnessDB != nil {
			_ = harnessDB.Close()
		}
	}

	// Step 9: Hand off to tray.Run() - this blocks until Quit.
	uiURL := fmt.Sprintf("http://localhost:%d", cfg.UI.Port)
	tray.Run(uiURL, onQuit)
	return nil
}

// startServices brings llama-server, embedder, queue, and metrics up under the
// current rt.cfg. Caller must hold rt.mu.
func (rt *runtime) startServices(
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
			)
		},
		HealthURL:      fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Model.Port),
		Events:         events,
		CheckPeriod:    5 * time.Second,
		HTTPClient:     httpclient.New(),
		OutputMaxLines: cfg.Log.ProcMaxLines,
	})
	go rt.llamaMgr.Run(ctx)

	rt.embedMgr = proc.NewManager(proc.ManagerConfig{
		Name: "embedder",
		BuildArgs: func() (string, []string) {
			return proc.EmbedderArgs(
				cfg.Embedder.Binary,
				cfg.Embedder.ModelPath,
				cfg.Embedder.Port,
			)
		},
		HealthURL:      fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Embedder.Port),
		Events:         events,
		CheckPeriod:    5 * time.Second,
		HTTPClient:     httpclient.New(),
		OutputMaxLines: cfg.Log.ProcMaxLines,
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

// applyConfig reloads config from the store, validates it, and either starts
// services for the first time or reconfigures the live ones to match. Tier-3
// changes (UI port, queue) are returned as RestartNeeded so the UI can flag
// them - no live apply path exists for those yet.
func (rt *runtime) applyConfig(
	ctx context.Context,
	uiServer *ui.Server,
	cfgStore config.Store,
	events chan proc.Event,
	metricsStore metrics.Store,
) ui.ApplyResult {
	uiServer.ClearStartupErrors()
	if cfgStore == nil {
		uiServer.AddStartupError(errConfigStoreUnavailable)
		return ui.ApplyResult{}
	}
	loaded, wasSaved, lerr := cfgStore.Load()
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
	validatePaths(uiServer, loaded)

	rt.mu.Lock()
	defer rt.mu.Unlock()

	old := rt.cfg
	rt.cfg = *loaded

	var result ui.ApplyResult

	if !rt.started {
		slog.Info("starting services", "model_port", loaded.Model.Port, "embed_port", loaded.Embedder.Port)
		rt.startServices(ctx, uiServer, events, metricsStore)
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
				)
			}, fmt.Sprintf("http://127.0.0.1:%d/health", loaded.Embedder.Port))
			result.LiveApplied = true
		}
		// The queue holds a reference to the inference client, which is pinned
		// to the model port - swap the client so in-flight requests drain
		// against the old port and new ones hit the new one.
		if old.Model.Port != loaded.Model.Port && rt.reqQueue != nil {
			rt.reqQueue.SetClient(inference.NewClient(
				fmt.Sprintf("http://127.0.0.1:%d", loaded.Model.Port),
				httpclient.NewStreaming(),
			))
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
		if rt.llamaMgr != nil {
			rt.llamaMgr.SetOutputMaxLines(loaded.Log.ProcMaxLines)
		}
		if rt.embedMgr != nil {
			rt.embedMgr.SetOutputMaxLines(loaded.Log.ProcMaxLines)
		}
		result.LiveApplied = true
	}

	return result
}

func (rt *runtime) getManagers() (*proc.Manager, *proc.Manager) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.llamaMgr, rt.embedMgr
}

// openDB opens harness.db (running migrations + seed) and returns the handle
// plus the typed sub-stores. Any failure is surfaced to the UI as a startup
// error; the returned handle and stores may be nil, which callers must handle.
func openDB(uiServer *ui.Server, path string) (*db.DB, config.Store, metrics.Store) {
	d, err := db.Open(path)
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("harness.db: %w", err))
		return nil, nil, nil
	}
	uiServer.SetConfigStore(d.Config())
	return d, d.Config(), d.Metrics()
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
		if _, err := os.Stat(c.path); errors.Is(err, fs.ErrNotExist) {
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

// teeWriter writes each payload to every underlying writer, discarding
// per-writer errors so one failing sink (e.g. a detached os.Stderr in a
// `-H windowsgui` build) cannot prevent the others from receiving the data.
// It always reports the full length written so slog's handler does not
// treat the write as short.
type teeWriter struct {
	writers []io.Writer
}

func tee(ws ...io.Writer) *teeWriter {
	return &teeWriter{writers: ws}
}

func (t *teeWriter) Write(p []byte) (int, error) {
	for _, w := range t.writers {
		_, _ = w.Write(p)
	}
	return len(p), nil
}

// recordMetrics periodically writes process and queue metrics to the store.
func recordMetrics(
	ctx context.Context,
	store metrics.Store,
	llamaMgr, embedMgr *proc.Manager,
	q *queue.Queue,
) {
	rec := metrics.NewRecorder(store)
	start := time.Now()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = rec.Uptime(time.Since(start))
			if q != nil {
				_ = rec.QueueDepth(q.Depth())
			}
			if llamaMgr != nil {
				st := llamaMgr.Status()
				_ = rec.ProcessHealth("llama-server", st.Healthy)
				_ = rec.ProcessRestartCount("llama-server", st.RestartCount)
			}
			if embedMgr != nil {
				st := embedMgr.Status()
				_ = rec.ProcessHealth("embedder", st.Healthy)
				_ = rec.ProcessRestartCount("embedder", st.RestartCount)
			}
		}
	}
}

// forwardEvents reads process events, logs them, and updates the UI state.
// Managers are fetched via getMgrs on every push so first-save startup is
// observed.
func forwardEvents(
	ctx context.Context,
	events <-chan proc.Event,
	uiSrv *ui.Server,
	getMgrs func() (*proc.Manager, *proc.Manager),
) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			logProcEvent(ev)
			l, e := getMgrs()
			pushStatus(uiSrv, l, e)
		case <-ticker.C:
			l, e := getMgrs()
			pushStatus(uiSrv, l, e)
		}
	}
}

// logProcEvent emits a slog entry for a proc.Event. Health-OK events log at
// debug level so the default Info handler doesn't spam the panel every 5s.
func logProcEvent(ev proc.Event) {
	attrs := []any{"process", ev.Process, "kind", string(ev.Kind), "msg", ev.Message}
	switch ev.Kind {
	case proc.EventHealthOK:
		slog.Debug("proc event", attrs...)
	case proc.EventHealthFail, proc.EventStop:
		slog.Warn("proc event", attrs...)
	case proc.EventError:
		slog.Error("proc event", attrs...)
	default:
		slog.Info("proc event", attrs...)
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
			ExitCode:     st.ExitCode,
			OutputTail:   st.OutputTail,
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
			ExitCode:     st.ExitCode,
			OutputTail:   st.OutputTail,
		})
	}
}
